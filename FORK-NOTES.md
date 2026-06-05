# Fork notes

Fork of [devgianlu/go-librespot](https://github.com/devgianlu/go-librespot) with
one addition on top of upstream, for the SoundTouch Reborn project
(https://github.com/JRpersonal/streborn).

## audio_output_pipe_passthrough (pipe backend)

Writes the raw Ogg/Vorbis bitstream to the output pipe untouched instead of
decoded PCM, so a downstream consumer (a hardware decoder) does the decoding.
Enable with `audio_backend: pipe` + `audio_output_pipe_passthrough: true`
(`audio_output_pipe_format` is then ignored).

Why: on weak ARM hardware that decodes Vorbis natively (the Bose SoundTouch
speakers STR revives), decoding to float32 PCM in go-librespot and re-streaming
PCM wastes CPU and bandwidth; passing the original Ogg through roughly halves CPU
on the box.

Caveats: no volume scaling / normalisation in passthrough (the stream is
untouched, use `external_volume` + downstream volume); seeking is limited to a
restart; pipe backend only; Ogg/Vorbis only (no FLAC passthrough).

Everything else tracks upstream. See the commit
"feat(pipe): add audio_output_pipe_passthrough ..." for the full diff.
