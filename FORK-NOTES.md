# Fork notes

Fork of [devgianlu/go-librespot](https://github.com/devgianlu/go-librespot)
for the SoundTouch Reborn project
(https://github.com/JRpersonal/streborn).

## pipe_passthrough backend

Writes the raw Ogg/Vorbis bitstream to the output pipe untouched instead of
decoded PCM, so a downstream consumer (a hardware decoder) does the decoding.
Enable with `audio_backend: pipe_passthrough` (`audio_output_pipe` must be
set as for the pipe backend; `audio_output_pipe_format` does not apply).

Deprecated alias: the feature used to be a mode of the pipe backend, enabled
with `audio_backend: pipe` + `audio_output_pipe_passthrough: true`. That
combination is still accepted and is normalized to the `pipe_passthrough`
backend at config load, with a deprecation warning in the log.

Why: on weak ARM hardware that decodes Vorbis natively (the Bose SoundTouch
speakers STR revives), decoding to float32 PCM in go-librespot and re-streaming
PCM wastes CPU and bandwidth; passing the original Ogg through roughly halves CPU
on the box.

Caveats: no volume scaling / normalisation in passthrough (the stream is
untouched, use `external_volume` + downstream volume); seeking is limited to a
restart (a mid-stream seek is reported as an error so controllers snap back to
the real position); crossfade is disabled under passthrough (mixing requires
decoded samples); named-pipe output only; Ogg/Vorbis only (no FLAC
passthrough).

## Robustness fixes on top of upstream

Candidates for upstreaming; kept as focused commits:

- spclient: the retry closure re-arms the request body on every attempt.
  Previously a retried request (401 token refresh, 5xx, network error) was
  sent with an empty body ("400 Missing payload", upstream issue #300).
- spclient/daemon: the connect-state PUT is bounded by its own deadline and
  the request retry budget is capped, so the daemon's single event loop can
  no longer block for minutes behind a wedged network.
- daemon: a failed state PUT (timeout, 5xx, network) keeps the state dirty
  and re-arms the coalescing timer with growing backoff, so the latest state
  converges instead of being lost.
- daemon: closed receiver channels are set to nil in the Run loop select
  instead of being re-selected, which busy-spun at 100% CPU.
- dealer: reconnection never gives up (capped interval instead of a
  ~15 minute budget) and runs with a real timeout context.
- tracks: a paged list whose pages could not be fetched no longer panics with
  "invalid paged list position: -1" (upstream issue #324); the daemon stops
  playback cleanly instead.
- audio: the chunked reader releases chunks the read position has long passed
  instead of holding the whole encrypted file until the track ends. Measured
  growth was about 1.5 MB of RSS per MB of audio, released only at the track
  change; a single 61 minute track drove a SoundTouch 20 from 29 MB free to
  3.8 MB free in 18 minutes. A window of 4 chunks (1 MiB) behind the position
  is kept for short backward seeks, the prefetch ahead is untouched, and a
  released chunk is re-downloaded transparently. Disabled while an OnComplete
  callback is registered, because that path re-reads the whole file to fill the
  audio cache.

Everything else tracks upstream.
