//go:build test_unit

package daemon

import (
	"testing"

	"github.com/devgianlu/go-librespot/dealer"
)

func TestRemoteCommandLogLine(t *testing.T) {
	type timer = struct {
		Type      string `json:"type"`
		DurationS int64  `json:"duration_s"`
	}

	tests := []struct {
		name     string
		endpoint string
		sentBy   string
		timer    *timer
		wantLine string
		wantInfo bool
	}{
		{"pause", "pause", "dev1", nil, "remote command pause from device dev1", true},
		{"resume", "resume", "dev1", nil, "remote command resume from device dev1", true},
		{"transfer", "transfer", "dev2", nil, "remote command transfer from device dev2", true},
		{"skip_next", "skip_next", "dev1", nil, "remote command skip_next from device dev1", true},
		{"unknown sender", "pause", "", nil, "remote command pause from device unknown", true},
		{"update_context is debug", "update_context", "dev1", nil, "remote command update_context from device dev1", false},
		{"set_queue is debug", "set_queue", "dev1", nil, "remote command set_queue from device dev1", false},
		{"sleep duration", "set_sleep_timer", "dev1", &timer{Type: "duration", DurationS: 1800}, "remote command set_sleep_timer (duration 1800s) from device dev1", true},
		{"sleep end of track", "set_sleep_timer", "dev1", &timer{Type: "end_of_track"}, "remote command set_sleep_timer (end_of_track) from device dev1", true},
		{"sleep clear", "set_sleep_timer", "dev1", &timer{Type: "clear"}, "remote command set_sleep_timer (clear) from device dev1", true},
		{"sleep no timer", "set_sleep_timer", "dev1", nil, "remote command set_sleep_timer (clear) from device dev1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req dealer.RequestPayload
			req.SentByDeviceId = tt.sentBy
			req.Command.Endpoint = tt.endpoint
			req.Command.TimerType = tt.timer

			line, info := remoteCommandLogLine(&req)
			if line != tt.wantLine {
				t.Errorf("line = %q, want %q", line, tt.wantLine)
			}
			if info != tt.wantInfo {
				t.Errorf("info = %v, want %v", info, tt.wantInfo)
			}
		})
	}
}
