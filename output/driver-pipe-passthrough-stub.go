//go:build windows

package output

import "fmt"

// The passthrough backend writes the raw encoded stream into a FIFO, which is
// POSIX-only, exactly like the plain pipe backend above it. STR runs it on a
// Linux speaker; this stub only keeps the Windows build honest.
func newPipePassthroughOutput(*NewOutputOptions) (Output, error) {
	return nil, fmt.Errorf("pipe_passthrough output is not supported on Windows")
}
