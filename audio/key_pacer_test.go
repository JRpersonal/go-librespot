//go:build test_unit

package audio

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/stretchr/testify/require"
)

// fakeClock lets the pacer's waits pass at once while still accounting for
// them, so the tests see exactly when each request would have gone out.
type fakeClock struct {
	mu    sync.Mutex
	start time.Time
	t     time.Time
}

func newFakeClock() *fakeClock {
	start := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &fakeClock{start: start, t: start}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

func (c *fakeClock) sinceStart() time.Duration {
	return c.now().Sub(c.start)
}

const answerDelay = 50 * time.Millisecond

// keyServer answers each request in turn from answers (true gives the key,
// false refuses it with code 1) and refuses once they run out. Each answer
// takes answerDelay to arrive.
type keyServer struct {
	clock *fakeClock

	mu      sync.Mutex
	answers []bool
	askedAt []time.Duration

	inFlight     atomic.Int32
	mostInFlight atomic.Int32
}

func (s *keyServer) send(context.Context) ([]byte, error) {
	s.mu.Lock()
	s.askedAt = append(s.askedAt, s.clock.sinceStart())
	give := len(s.answers) > 0 && s.answers[0]
	if len(s.answers) > 0 {
		s.answers = s.answers[1:]
	}
	s.mu.Unlock()

	n := s.inFlight.Add(1)
	for {
		most := s.mostInFlight.Load()
		if n <= most || s.mostInFlight.CompareAndSwap(most, n) {
			break
		}
	}
	s.clock.advance(answerDelay)
	s.inFlight.Add(-1)

	if give {
		return []byte("0123456789abcdef"), nil
	}
	return nil, &KeyProviderError{Code: 1}
}

func (s *keyServer) asked() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.askedAt...)
}

func newTestPacer(answers ...bool) (*keyPacer, *keyServer, *fakeClock) {
	clock := newFakeClock()
	pacer := newKeyPacer(&librespot.NullLogger{}, defaultKeyPacerConfig)
	pacer.now = clock.now
	pacer.sleep = clock.sleep
	return pacer, &keyServer{clock: clock, answers: answers}, clock
}

func ms(v ...int) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, m := range v {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}

func TestKeyPacerSendsOneRequestAtATimeAndSpacesThem(t *testing.T) {
	pacer, server, _ := newTestPacer(true, true, true, true)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pacer.request(context.Background(), server.send)
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	require.EqualValues(t, 1, server.mostInFlight.Load())
	// Each waits for the previous answer, then for the gap since it was sent.
	require.Equal(t, ms(0, 200, 400, 600), server.asked())
}

func TestKeyPacerAsksAgainForARefusedKeyAfterAGrowingWait(t *testing.T) {
	pacer, server, _ := newTestPacer(false, false, false, true)

	key, err := pacer.request(context.Background(), server.send)
	require.NoError(t, err)
	require.NotNil(t, key)

	// Refused at 50, 3100 and 8150 ms: waits of 3, 5 and 10 s.
	require.Equal(t, ms(0, 3050, 8100, 18150), server.asked())
	require.False(t, pacer.backingOff(), "a key that arrives ends the wait")
}

func TestKeyPacerReturnsAKeyRefusedThroughEveryRetry(t *testing.T) {
	pacer, server, _ := newTestPacer()

	_, err := pacer.request(context.Background(), server.send)

	var keyErr *KeyProviderError
	require.ErrorAs(t, err, &keyErr)
	require.EqualValues(t, 1, keyErr.Code)
	require.Len(t, server.asked(), defaultKeyPacerConfig.retries+1)
	require.True(t, pacer.backingOff())
}

func TestKeyPacerHoldsBackEveryKeyOfTheSessionAfterARefusal(t *testing.T) {
	pacer, server, _ := newTestPacer(false, true, true)

	_, err := pacer.tryOnce(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))

	// Another track's key waits for the same back-off.
	_, err = pacer.request(context.Background(), server.send)
	require.NoError(t, err)
	require.Equal(t, ms(0, 3050), server.asked())

	// The key that arrived ended it: the next one only keeps the gap.
	_, err = pacer.request(context.Background(), server.send)
	require.NoError(t, err)
	require.Equal(t, ms(0, 3050, 3250), server.asked())
}

// The skip walk: after a key has been refused through all its retries, the
// daemon skips to the next track. Its key still waits out the back-off, so the
// walk cannot burst, but it is asked for once: the run has outlasted every wait
// already, and retrying again would only stretch the silence.
func TestKeyPacerAsksOnceForKeysOfARunThatOutlastedItsRetries(t *testing.T) {
	pacer, server, _ := newTestPacer()

	_, err := pacer.request(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))

	_, err = pacer.request(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))
	_, err = pacer.request(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))

	require.Equal(t, ms(0, 3050, 8100, 18150, 28200, 38250), server.asked())
}

func TestKeyPacerStartsANewRunAfterAQuietSpell(t *testing.T) {
	pacer, server, clock := newTestPacer()

	_, err := pacer.request(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))
	require.Len(t, server.asked(), 4)

	// Nothing asked for a while: the next refusal is a new run with its own
	// retries, starting again at the shortest wait.
	clock.advance(defaultKeyPacerConfig.runQuiet)
	start := clock.sinceStart()

	server.mu.Lock()
	server.answers = []bool{false, true}
	server.mu.Unlock()

	_, err = pacer.request(context.Background(), server.send)
	require.NoError(t, err)

	asked := server.asked()
	require.Len(t, asked, 6)
	require.Equal(t, start, asked[4])
	require.Equal(t, start+answerDelay+3*time.Second, asked[5])
}

func TestKeyPacerSingleAttemptIsNotSentDuringABackOff(t *testing.T) {
	pacer, server, clock := newTestPacer(false, true)

	_, err := pacer.tryOnce(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))

	_, err = pacer.tryOnce(context.Background(), server.send)
	require.ErrorIs(t, err, ErrKeyBackingOff)
	require.False(t, isKeyRefusal(err), "backing off is not a refusal of this key")
	require.Equal(t, ms(0), server.asked())

	clock.advance(3 * time.Second)
	_, err = pacer.tryOnce(context.Background(), server.send)
	require.NoError(t, err)
}

func TestKeyPacerSingleAttemptDoesNotRetry(t *testing.T) {
	pacer, server, _ := newTestPacer(false, true)

	_, err := pacer.tryOnce(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))
	require.Len(t, server.asked(), 1)
}

func TestKeyPacerTimeoutDoesNotStartABackOff(t *testing.T) {
	pacer, _, _ := newTestPacer()

	_, err := pacer.request(context.Background(), func(context.Context) ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, pacer.backingOff())
}

func TestKeyPacerCancelledWaitSendsNothingAndFreesTheTurn(t *testing.T) {
	pacer, server, _ := newTestPacer(false, true)

	_, err := pacer.tryOnce(context.Background(), server.send)
	require.True(t, isKeyRefusal(err))

	// A load the player has moved on from stops waiting without asking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pacer.request(ctx, server.send)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, server.asked(), 1)

	// And the next request is not stuck behind it.
	_, err = pacer.request(context.Background(), server.send)
	require.NoError(t, err)
	require.Equal(t, ms(0, 3050), server.asked())
}

func TestKeyPacerRealSleepEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := sleepContext(ctx, time.Minute)
	require.True(t, errors.Is(err, context.Canceled))
	require.Less(t, time.Since(start), 30*time.Second)
}

func TestSingleKeyAttemptMarksTheContext(t *testing.T) {
	require.False(t, isSingleKeyAttempt(context.Background()))
	require.True(t, isSingleKeyAttempt(WithSingleKeyAttempt(context.Background())))
}
