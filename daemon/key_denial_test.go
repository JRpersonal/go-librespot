//go:build test_unit

package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/audio"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/stretchr/testify/require"
)

func keyDenial() error {
	return fmt.Errorf("failed retrieving audio key: %w", &audio.KeyProviderError{Code: 1})
}

// Scattered refusals are single unavailable tracks and keep being skipped:
// only refusals in a row add up to a session-level denial.
func TestOnlyConsecutiveKeyDenialsStopPlayback(t *testing.T) {
	p := newTestAppPlayer(t)

	for i := 0; i < 5; i++ {
		p.noteLoadResult(keyDenial())
		p.noteLoadResult(keyDenial())
		require.False(t, p.keyDenialStreakReached(), "two refusals in a row are still track-level")
		p.noteLoadResult(nil)
	}

	// Failures that say nothing about the key neither extend nor end a streak.
	p.noteLoadResult(keyDenial())
	p.noteLoadResult(fmt.Errorf("load: %w", librespot.ErrMediaRestricted))
	p.noteLoadResult(keyDenial())
	require.False(t, p.keyDenialStreakReached())
	p.noteLoadResult(keyDenial())
	require.True(t, p.keyDenialStreakReached())
}

// The streak ends in one stop and one clear session-level error, not in a walk
// through the rest of the context.
func TestKeyDenialStreakStopsWithOneSessionError(t *testing.T) {
	p := newTestAppPlayer(t)
	p.state.player.IsPlaying = true
	p.state.player.IsBuffering = true

	for i := 0; i < maxConsecutiveKeyDenials; i++ {
		p.noteLoadResult(keyDenial())
	}
	require.True(t, p.keyDenialStreakReached())

	now := time.Now()
	err := p.abortOnKeyDenials("spotify:track:0", keyDenial(), now)
	require.ErrorIs(t, err, ErrAudioKeyDenied)
	require.False(t, isUnplayableMedia(err), "the session-level error must not trigger another skip")

	require.False(t, p.state.player.IsPlaying)
	require.False(t, p.state.player.IsBuffering)
	require.Zero(t, p.keyDenial.streak)

	emitted := p.app.server.(*recordingApiServer).emitted
	require.Len(t, emitted, 1)
	data := emitted[0].Data.(ApiEventDataPlaybackError)
	require.Equal(t, playbackErrorStageSession, data.Stage)
	require.Equal(t, playbackErrorKindKeyDenied, data.Kind)
	require.False(t, data.Unplayable)
}

// While backing off, a load is refused before any key request can go out.
func TestLoadsAreHeldBackAfterAKeyDenialStreak(t *testing.T) {
	p := newTestAppPlayer(t)
	p.state.player.Track = &connectpb.ProvidedTrack{Uri: "spotify:track:6rqhFgbbKwnb9MLmUQDhG6"}

	now := time.Now()
	for i := 0; i < maxConsecutiveKeyDenials; i++ {
		p.noteLoadResult(keyDenial())
	}
	_ = p.abortOnKeyDenials("spotify:track:0", keyDenial(), now)

	var got error
	p.loadCurrentTrack(false, true, false, 0, func(err error) { got = err })
	require.ErrorIs(t, got, ErrAudioKeyDenied)
	require.Empty(t, p.loader.queue, "a held-back load must not reach the loader")

	require.Error(t, p.keyDenialHeldBack(now.Add(keyDenialBackoffMin-time.Second)))
	require.NoError(t, p.keyDenialHeldBack(now.Add(keyDenialBackoffMin)))
}

// Every further session-level denial backs off longer, up to a cap, and a
// track that plays clears the back-off entirely.
func TestKeyDenialBackoffGrowsAndClearsOnSuccess(t *testing.T) {
	p := newTestAppPlayer(t)
	now := time.Now()

	var backoffs []time.Duration
	for i := 0; i < 6; i++ {
		_ = p.abortOnKeyDenials("spotify:track:0", keyDenial(), now)
		backoffs = append(backoffs, p.keyDenial.backoff)
	}
	require.Equal(t, []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}, backoffs)

	p.noteLoadResult(nil)
	require.NoError(t, p.keyDenialHeldBack(now))
	_ = p.abortOnKeyDenials("spotify:track:0", keyDenial(), now)
	require.Equal(t, keyDenialBackoffMin, p.keyDenial.backoff)
}

func TestKeyDenialClassifiesApartFromARestrictedTrack(t *testing.T) {
	require.Equal(t, playbackErrorKindKeyDenied, classifyPlaybackError(fmt.Errorf("x: %w", ErrAudioKeyDenied)))
	require.Equal(t, playbackErrorKindRestricted, classifyPlaybackError(keyDenial()))
	require.False(t, errors.Is(keyDenial(), ErrAudioKeyDenied))
}
