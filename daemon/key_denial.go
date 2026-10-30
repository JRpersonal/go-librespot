package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/devgianlu/go-librespot/audio"
	"github.com/devgianlu/go-librespot/mpris"
)

// When Spotify refuses the AES audio key for one track, skipping past it is
// right: a single track can be unavailable in a region or unlicensed for this
// kind of client. When it refuses the key for several tracks IN A ROW, the
// refusal is about the session, not the track, and skipping on only walks the
// rest of the context at several tracks a second. Every one of those skips is
// another key request, which feeds Spotify's per-account throttle (429) and
// keeps the refusal going (streborn #480: 51 tracks per preset press).
//
// So a streak of refusals is treated as a session-level failure: playback stops,
// one clear playback_error goes out, and new loads are held back for a while.
const (
	// maxConsecutiveKeyDenials is how many audio-key refusals in a row end the
	// walk. Only consecutive refusals count: a successful load resets the
	// streak, so a playlist with a few scattered blocked tracks still skips past
	// them as before.
	maxConsecutiveKeyDenials = 3

	// keyDenialBackoffMin is how long loads are refused after the first
	// session-level denial. Each further one doubles it, up to
	// keyDenialBackoffMax. A successful load clears it.
	keyDenialBackoffMin = 30 * time.Second
	keyDenialBackoffMax = 5 * time.Minute
)

// ErrAudioKeyDenied marks playback stopped because Spotify refused the audio key
// for several tracks in a row, or a load held back because of it. It
// deliberately does not wrap the underlying *audio.KeyProviderError: anything
// that matches that one skips to the next track, and this must not.
var ErrAudioKeyDenied = errors.New("spotify refused the audio key for several tracks in a row")

// keyDenialState tracks the current streak of audio-key refusals and the
// back-off that follows a session-level denial. Only touched on the player loop.
type keyDenialState struct {
	streak       int
	backoff      time.Duration
	backoffUntil time.Time
}

// noteLoadResult updates the refusal streak with the outcome of a track load.
// A refused key extends the streak, a successful load ends it and clears any
// back-off. Other failures (restricted media, network) say nothing about the
// key and leave the streak as it is.
func (p *AppPlayer) noteLoadResult(err error) {
	if err == nil {
		p.keyDenial = keyDenialState{}
		return
	}

	var keyErr *audio.KeyProviderError
	if errors.As(err, &keyErr) {
		p.keyDenial.streak++
	}
}

// keyDenialStreakReached reports whether the refusals so far amount to a
// session-level denial, so the caller must stop instead of skipping on.
func (p *AppPlayer) keyDenialStreakReached() bool {
	return p.keyDenial.streak >= maxConsecutiveKeyDenials
}

// keyDenialHeldBack returns an error while loads are being held back after a
// session-level denial, nil otherwise.
func (p *AppPlayer) keyDenialHeldBack(now time.Time) error {
	if p.keyDenial.backoffUntil.IsZero() || !now.Before(p.keyDenial.backoffUntil) {
		return nil
	}

	return fmt.Errorf("%w: not asking again for %s", ErrAudioKeyDenied, p.keyDenial.backoffUntil.Sub(now).Round(time.Second))
}

// abortOnKeyDenials stops playback after a streak of refused audio keys, starts
// the back-off and tells API clients once, with the session-level error that is
// returned. last is the refusal that completed the streak.
func (p *AppPlayer) abortOnKeyDenials(uri string, last error, now time.Time) error {
	streak := p.keyDenial.streak

	backoff := keyDenialBackoffMin
	if p.keyDenial.backoff > 0 {
		backoff = min(2*p.keyDenial.backoff, keyDenialBackoffMax)
	}
	p.keyDenial = keyDenialState{backoff: backoff, backoffUntil: now.Add(backoff)}
	p.consecutiveUnplayableSkips = 0

	err := fmt.Errorf("%w (%d in a row, last: %v), stopping playback and holding back new loads for %s",
		ErrAudioKeyDenied, streak, last, backoff)
	p.app.log.Errorf("stopping playback: %v", err)

	// Nothing is playing: the failed load left the state buffering.
	p.player.Stop()
	p.primaryStream = nil
	p.state.player.IsPlaying = false
	p.state.player.IsPaused = false
	p.state.player.IsBuffering = false
	p.updateState()
	p.emitMprisUpdate(mpris.Stopped)

	p.emitPlaybackError(playbackErrorStageSession, uri, err)
	return err
}
