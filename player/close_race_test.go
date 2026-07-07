//go:build !windows

// Regression for the production "panic: send on closed channel" caught
// during Spotify transfer storms: playback got transferred away (zeroconf
// new-user -> AppPlayer.Close) while API play/stop/volume commands were
// still in flight. manageLoop used to close(p.cmd) on exit — the receiver
// closing a channel other goroutines send on. Now manageLoop closes only
// its done channel and every sender selects on it.
//
// The pipe output runs against a real unix FIFO in t.TempDir, so Stop/Close
// race actual driver-pipe writes, mirroring the deployed pipe backend.
package player

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// silenceSource is an endless AudioSource so the pipe output always has
// samples to write while commands race.
type silenceSource struct{}

func (s *silenceSource) Read(p []float32) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
func (s *silenceSource) SetPositionMs(int64) error { return nil }
func (s *silenceSource) PositionMs() int64         { return 0 }

// newPipePlayer builds a Player on the pipe backend writing into a FIFO
// that a background goroutine keeps draining (the pulsar node's role).
func newPipePlayer(t *testing.T) *Player {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "audio.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	// Reader end first: openPipeForWriting fails fast when no reader is
	// attached. Non-blocking so the drainer can be shut down cleanly.
	rd, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 64*1024)
		for {
			if _, err := rd.Read(buf); err != nil {
				if errors.Is(err, os.ErrClosed) {
					return
				}
				// EOF (writer swapped) / EAGAIN: keep the reader end alive.
				time.Sleep(time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() {
		_ = rd.Close()
		<-drainDone
	})

	p, err := NewPlayer(&Options{
		Log:                   &librespot.NullLogger{},
		AudioBackend:          "pipe",
		AudioOutputPipe:       fifo,
		AudioOutputPipeFormat: "f32le",
		VolumeUpdate:          make(chan float32, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRapidStopVolumeCloseNoPanic(t *testing.T) {
	for iter := 0; iter < 8; iter++ {
		p := newPipePlayer(t)

		// Drain player events like AppPlayer.Run would; without a consumer
		// the 128-slot buffer fills and manageLoop stalls by design.
		evDone := make(chan struct{})
		go func() {
			for {
				select {
				case <-p.Receive():
				case <-evDone:
					return
				}
			}
		}()

		var wg sync.WaitGroup
		stop := make(chan struct{})
		hammer := func(f func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						f()
					}
				}
			}()
		}

		vol := uint32(0)
		hammer(func() {
			vol = (vol + 4096) % (MaxStateVolume + 1)
			p.SetVolume(vol)
		})
		hammer(func() {
			_ = p.Play()
			_ = p.Pause()
		})
		hammer(func() { _ = p.PositionMs() })
		hammer(func() {
			// Fresh stream (recreates the pipe output and its write loop),
			// then a rapid stop — the transfer-storm shape.
			_ = p.SetPrimaryStream(&silenceSource{}, false, false)
			p.Stop()
		})

		time.Sleep(20 * time.Millisecond) // let the storm develop

		closed := make(chan struct{})
		go func() {
			p.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("player.Close deadlocked during the command storm")
		}

		close(stop)
		joined := make(chan struct{})
		go func() {
			wg.Wait()
			close(joined)
		}()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Fatal("api goroutines wedged after Close (send-after-close regression)")
		}

		// After close every command is inert: typed error or zero value,
		// never a panic, never a hang.
		if err := p.Play(); !errors.Is(err, ErrPlayerClosed) {
			t.Fatalf("Play after close: %v, want ErrPlayerClosed", err)
		}
		if err := p.Pause(); !errors.Is(err, ErrPlayerClosed) {
			t.Fatalf("Pause after close: %v, want ErrPlayerClosed", err)
		}
		if err := p.SeekMs(1000); !errors.Is(err, ErrPlayerClosed) {
			t.Fatalf("SeekMs after close: %v, want ErrPlayerClosed", err)
		}
		if err := p.SetPrimaryStream(&silenceSource{}, true, false); !errors.Is(err, ErrPlayerClosed) {
			t.Fatalf("SetPrimaryStream after close: %v, want ErrPlayerClosed", err)
		}
		if pos := p.PositionMs(); pos != 0 {
			t.Fatalf("PositionMs after close: %d, want 0", pos)
		}
		p.SetVolume(1000)                      // must not panic
		p.Stop()                               // must not panic
		p.SetSecondaryStream(&silenceSource{}) // must not panic
		p.Close()                              // idempotent

		close(evDone)
	}
}

// TestCloseRacesClose: two goroutines closing simultaneously — only one
// playerCmdClose can be accepted, the other must fall through done.
func TestCloseRacesClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := NewPlayer(&Options{
			Log:          &librespot.NullLogger{},
			AudioBackend: "pipe", // never instantiated: no stream is set
			VolumeUpdate: make(chan float32, 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.Close()
			}()
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close calls deadlocked")
		}
	}
}
