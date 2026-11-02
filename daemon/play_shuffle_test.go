//go:build test_unit

package daemon

import (
	"context"
	"fmt"
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/session"
	"github.com/stretchr/testify/require"
)

const shufflePlaylistUri = "spotify:playlist:37i9dQZF1DX8vwRmUsEIMT"

func boolPtr(v bool) *bool { return &v }

// shufflePlaylist is a context with its tracks already in hand, so loading it
// never reaches for the network.
func shufflePlaylist(restrictions *connectpb.Restrictions) *connectpb.Context {
	var page []*connectpb.ContextTrack
	for i := range 40 {
		// Base62 ids of 22 characters, distinct per track.
		page = append(page, &connectpb.ContextTrack{
			Uri: fmt.Sprintf("spotify:track:%022d", i),
			Uid: fmt.Sprintf("u%d", i),
		})
	}

	return &connectpb.Context{
		Uri:          shufflePlaylistUri,
		Restrictions: restrictions,
		Pages:        []*connectpb.ContextPage{{Tracks: page}},
	}
}

// playToFirstTrack sends a /player/play and runs the work it starts up to the
// point where the track it landed on is about to be loaded. It returns that
// track.
func playToFirstTrack(t *testing.T, p *AppPlayer, spotCtx *connectpb.Context, data ApiPlay) string {
	t.Helper()

	p.sess = &session.Session{}
	p.resolvePlayContext = func(context.Context, string) (*connectpb.Context, error) { return spotCtx, nil }

	req, _ := NewApiRequest(ApiRequestTypePlay, data)
	_, err := p.handleApiRequest(req)
	require.ErrorIs(t, err, errReplyDeferred)

	// A play only resolves the context: setting shuffle walks nothing.
	require.Len(t, p.loader.queue, 1)
	require.Equal(t, "resolve "+data.Uri, p.loader.queue[0].name)
	runQueuedJob(t, p)

	require.Len(t, p.loader.queue, 1)
	require.Equal(t, "resolve "+spotCtx.Uri, p.loader.queue[0].name)
	runQueuedJob(t, p)

	current := p.state.tracks.CurrentTrack().GetUri()
	var loads []string
	for _, job := range p.loader.queue {
		loads = append(loads, job.name)
	}
	require.Contains(t, loads, "load "+current)
	return current
}

func shuffleEvents(p *AppPlayer) []bool {
	var values []bool
	for _, ev := range p.app.server.(*recordingApiServer).emitted {
		if ev.Type == ApiEventTypeShuffleContext {
			values = append(values, ev.Data.(ApiEventDataShuffleContext).Value)
		}
	}
	return values
}

// A play that asks for shuffle starts on a random track with the one load,
// rather than loading the first track and skipping away from it.
func TestPlayWithShuffleStartsOnARandomTrack(t *testing.T) {
	first := shufflePlaylist(nil).Pages[0].Tracks[0].Uri
	shuffle := true

	elsewhere := 0
	for range 8 {
		p := newTestAppPlayer(t)
		current := playToFirstTrack(t, p, shufflePlaylist(nil), ApiPlay{
			Uri:            shufflePlaylistUri,
			Paused:         true,
			ShuffleContext: &shuffle,
		})

		require.True(t, p.state.player.Options.ShufflingContext)
		require.Equal(t, []bool{true}, shuffleEvents(p))
		if current != first {
			elsewhere++
		}
	}

	// Eight starts on the first of 40 tracks would happen once in 6.5e12.
	require.Positive(t, elsewhere, "every shuffled play started on the first track")
}

// Shuffle left on by an earlier play does not carry into a play that asks for
// the context in order.
func TestPlayWithoutShuffleStartsOnTheFirstTrack(t *testing.T) {
	p := newTestAppPlayer(t)
	p.state.player.Options.ShufflingContext = true
	shuffle := false

	current := playToFirstTrack(t, p, shufflePlaylist(nil), ApiPlay{
		Uri:            shufflePlaylistUri,
		ShuffleContext: &shuffle,
	})

	require.Equal(t, shufflePlaylist(nil).Pages[0].Tracks[0].Uri, current)
	require.False(t, p.state.player.Options.ShufflingContext)
	require.Equal(t, []bool{false}, shuffleEvents(p))
}

// Without shuffle_context a play keeps the setting it finds, as before.
func TestPlayWithoutShuffleContextKeepsTheSetting(t *testing.T) {
	p := newTestAppPlayer(t)
	p.state.player.Options.ShufflingContext = true

	playToFirstTrack(t, p, shufflePlaylist(nil), ApiPlay{Uri: shufflePlaylistUri})

	require.True(t, p.state.player.Options.ShufflingContext)
	require.Empty(t, shuffleEvents(p))
}

// A context that may not be shuffled plays in order, and says so.
func TestPlayWithShuffleOfAContextThatForbidsIt(t *testing.T) {
	restricted := &connectpb.Restrictions{DisallowTogglingShuffleReasons: []string{"not_supported"}}
	p := newTestAppPlayer(t)
	shuffle := true

	current := playToFirstTrack(t, p, shufflePlaylist(restricted), ApiPlay{
		Uri:            shufflePlaylistUri,
		ShuffleContext: &shuffle,
	})

	require.Equal(t, shufflePlaylist(nil).Pages[0].Tracks[0].Uri, current)
	require.False(t, p.state.player.Options.ShufflingContext)
	require.Empty(t, shuffleEvents(p))
}

// Asking for the setting already in place announces nothing.
func TestPlayWithUnchangedShuffleEmitsNothing(t *testing.T) {
	p := newTestAppPlayer(t)
	p.state.player.Options.ShufflingContext = true
	shuffle := true

	playToFirstTrack(t, p, shufflePlaylist(nil), ApiPlay{Uri: shufflePlaylistUri, ShuffleContext: &shuffle})

	require.True(t, p.state.player.Options.ShufflingContext)
	require.Empty(t, shuffleEvents(p))
}

// A play superseded before its context resolved leaves the setting alone.
func TestSupersededPlayLeavesShuffleAlone(t *testing.T) {
	p := newTestAppPlayer(t)
	p.sess = &session.Session{}
	p.resolvePlayContext = func(context.Context, string) (*connectpb.Context, error) { return shufflePlaylist(nil), nil }
	shuffle := true

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiPlay{Uri: shufflePlaylistUri, ShuffleContext: &shuffle})
	_, err := p.handleApiRequest(req)
	require.ErrorIs(t, err, errReplyDeferred)

	// Whatever came next moved the load generation on.
	p.loadGen++
	runQueuedJob(t, p)

	require.False(t, p.state.player.Options.ShufflingContext)
	require.Empty(t, shuffleEvents(p))
}

// GET / tells a client that this build takes shuffle_context on a play, with
// or without a session.
func TestApiRootListsFeatures(t *testing.T) {
	require.Contains(t, newApiRoot(false).Features, ApiFeaturePlayShuffleContext)

	p := newTestAppPlayer(t)
	resp, err := p.handleApiRequest(ApiRequest{Type: ApiRequestTypeRoot})
	require.NoError(t, err)
	require.Contains(t, resp.(*ApiRoot).Features, ApiFeaturePlayShuffleContext)
}
