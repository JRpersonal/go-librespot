//go:build test_unit

package main

import (
	"testing"

	"github.com/devgianlu/go-librespot/output"
	"github.com/stretchr/testify/require"
)

// TestNormalizeAudioBackend covers the backward-compat shim for the
// deprecated audio_output_pipe_passthrough flag: an old-style configuration
// (audio_backend: pipe plus the flag) must select the pipe_passthrough
// backend, a new-style configuration must pass through untouched, and the
// plain pipe backend without the flag must stay pipe.
func TestNormalizeAudioBackend(t *testing.T) {
	cases := []struct {
		name        string
		backend     string
		passthrough bool
		want        string
		wantLegacy  bool
	}{
		{"old-style pipe with flag", "pipe", true, output.BackendPipePassthrough, true},
		{"new-style backend", output.BackendPipePassthrough, false, output.BackendPipePassthrough, false},
		{"plain pipe without flag", "pipe", false, "pipe", false},
		{"new-style backend with redundant flag", output.BackendPipePassthrough, true, output.BackendPipePassthrough, false},
		{"other backend with flag", "alsa", true, "alsa", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, legacy := normalizeAudioBackend(tc.backend, tc.passthrough)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantLegacy, legacy)
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1GB", 1 << 30, false},
		{"1gb", 1 << 30, false},
		{"500MB", 500 << 20, false},
		{"512KB", 512 << 10, false},
		{"2TB", 2 << 40, false},
		{"1024", 1024, false},
		{"1024B", 1024, false},
		{"1.5GB", 1610612736, false},
		{" 256MB ", 256 << 20, false},
		{"abc", 0, true},
		{"-1GB", 0, true},
		{"GB", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
