//go:build test_unit

package audio

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sliding window in releaseChunksBehind is what keeps a long track from
// pinning its whole encrypted file in the ~120 MB of RAM a SoundTouch speaker
// has. These tests drive a reader over a synthetic multi-chunk file served by a
// local httptest server (Range requests only, no network) and pin down the four
// properties the window has to have.

// rangeServer serves testData for Range requests and counts how often each
// chunk was requested, so a re-download after an eviction is observable.
type rangeServer struct {
	*httptest.Server

	testData []byte

	mu       sync.Mutex
	requests map[int]int
}

func newRangeServer(t *testing.T, chunks int, extra int) *rangeServer {
	t.Helper()

	s := &rangeServer{
		testData: make([]byte, chunks*DefaultChunkSize+extra),
		requests: map[int]int{},
	}
	for i := range s.testData {
		s.testData[i] = byte(i % 251)
	}

	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	var start, end int64
	if n, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n != 2 || err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if start < 0 || start > end || start >= int64(len(s.testData)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= int64(len(s.testData)) {
		end = int64(len(s.testData)) - 1
	}

	s.mu.Lock()
	s.requests[int(start/DefaultChunkSize)]++
	s.mu.Unlock()

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.testData)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(s.testData[start : end+1])
}

func (s *rangeServer) requestCount(chunkIdx int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.requests[chunkIdx]
}

func (s *rangeServer) newReader(t *testing.T) *HttpChunkedReader {
	t.Helper()

	reader, err := NewHttpChunkedReader(&librespot.NullLogger{}, s.Client(), s.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

// residentChunks reports which chunk payloads are currently held in memory.
func residentChunks(r *HttpChunkedReader) []int {
	var resident []int
	for idx, chunk := range r.chunks {
		chunk.L.Lock()
		if chunk.data != nil {
			resident = append(resident, idx)
		}
		chunk.L.Unlock()
	}

	return resident
}

func chunkResident(r *HttpChunkedReader, idx int) bool {
	chunk := r.chunks[idx]
	chunk.L.Lock()
	defer chunk.L.Unlock()

	return chunk.data != nil
}

// readSequentially reads the whole file in bufSize steps, calling onProgress
// after every read, and returns everything it read.
func readSequentially(t *testing.T, r *HttpChunkedReader, bufSize int, onProgress func()) []byte {
	t.Helper()

	var out []byte
	buf := make([]byte, bufSize)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if onProgress != nil {
			onProgress()
		}

		if err == io.EOF {
			return out
		}

		require.NoError(t, err)
		if n == 0 {
			t.Fatal("read made no progress")
		}
	}
}

func TestReleaseChunksBehindKeepsWindowAndPrefetch(t *testing.T) {
	const chunks = 12
	server := newRangeServer(t, chunks, 0)
	reader := server.newReader(t)

	// Read up to the start of chunk 8, then a short read inside it, so the
	// last read starts in chunk 8 and prefetches 9 and 10 from there.
	const currIdx = 8
	_, err := reader.Read(make([]byte, currIdx*DefaultChunkSize))
	require.NoError(t, err)
	_, err = reader.Read(make([]byte, 1024))
	require.NoError(t, err)

	// Everything more than RetainedChunksBehind behind the position is gone.
	for idx := 0; idx <= currIdx-RetainedChunksBehind-1; idx++ {
		assert.Falsef(t, chunkResident(reader, idx), "chunk %d should have been released", idx)
	}

	// The window behind the position and the current chunk are kept, so a
	// short backward seek does not re-download.
	for idx := currIdx - RetainedChunksBehind; idx <= currIdx; idx++ {
		assert.Truef(t, chunkResident(reader, idx), "chunk %d is inside the window and must be kept", idx)
	}

	// The prefetch ahead is untouched (it lands asynchronously).
	require.Eventually(t, func() bool {
		return chunkResident(reader, currIdx+1) && chunkResident(reader, currIdx+2)
	}, time.Second, 10*time.Millisecond, "prefetched chunks ahead of the position must be kept")
}

func TestReleasedChunkIsReDownloadedTransparently(t *testing.T) {
	const chunks = 12
	server := newRangeServer(t, chunks, 0)
	reader := server.newReader(t)

	// Walk far enough forward that chunk 0 falls out of the window.
	_, err := reader.Read(make([]byte, 8*DefaultChunkSize))
	require.NoError(t, err)
	require.False(t, chunkResident(reader, 0), "chunk 0 should have been released")
	require.Equal(t, 1, server.requestCount(0))

	// Reading it again must return the same bytes, from a fresh download.
	buf := make([]byte, DefaultChunkSize)
	n, err := reader.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, DefaultChunkSize, n)
	assert.Equal(t, server.testData[:DefaultChunkSize], buf)
	assert.Equal(t, 2, server.requestCount(0), "the released chunk must have been downloaded again")
}

func TestNoReleaseWhileCompletionCallbackRegistered(t *testing.T) {
	const chunks = 12
	server := newRangeServer(t, chunks, 0)
	reader := server.newReader(t)

	// player.go registers this to persist the complete encrypted file to the
	// audio cache; the callback re-reads the whole file through the reader, so
	// nothing may be released behind it.
	complete := make(chan []byte, 1)
	reader.OnComplete(func(r io.ReaderAt, size int64) {
		data, err := io.ReadAll(io.NewSectionReader(r, 0, size))
		assert.NoError(t, err)
		complete <- data
	})

	got := readSequentially(t, reader, 64*1024, nil)
	assert.Equal(t, server.testData, got)

	for idx := range reader.chunks {
		assert.Truef(t, chunkResident(reader, idx), "chunk %d must be kept while a completion callback is registered", idx)
	}

	select {
	case data := <-complete:
		assert.Equal(t, server.testData, data, "the completion callback must see the whole file")
	case <-time.After(5 * time.Second):
		t.Fatal("completion callback did not fire")
	}

	// Every chunk was downloaded exactly once: nothing was released, so
	// nothing had to be fetched twice and completedChunks is not inflated.
	for idx := range reader.chunks {
		assert.Equalf(t, 1, server.requestCount(idx), "chunk %d should have been downloaded once", idx)
	}
	reader.completeMu.Lock()
	completed := reader.completedChunks
	reader.completeMu.Unlock()
	assert.Equal(t, len(reader.chunks), completed)
}

func TestResidentChunksStayBoundedOverLongRead(t *testing.T) {
	// 32 chunks are 8 MiB, the scale at which the old reader ate the speaker's
	// RAM: without the window every chunk would still be resident at the end.
	const chunks = 32
	server := newRangeServer(t, chunks, 1000)
	reader := server.newReader(t)

	// Current chunk + window behind + prefetch ahead, plus one for a read that
	// straddles a chunk boundary while a prefetch is still landing.
	maxResident := 1 + RetainedChunksBehind + PrefetchCount + 1

	peak := 0
	got := readSequentially(t, reader, 32*1024, func() {
		if n := len(residentChunks(reader)); n > peak {
			peak = n
		}
	})

	require.Equal(t, server.testData, got, "the bounded reader must still deliver the whole file")
	t.Logf("peak resident chunks over %d chunks read: %d (%d KiB)", chunks, peak, peak*DefaultChunkSize/1024)
	assert.LessOrEqualf(t, peak, maxResident,
		"resident chunks peaked at %d (%d KiB), expected at most %d", peak, peak*DefaultChunkSize/1024, maxResident)
	assert.Less(t, len(residentChunks(reader)), len(reader.chunks),
		"the reader must not still hold the whole file at the end of the track")
}
