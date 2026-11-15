//go:build !linux

package urnettools

import (
	"fmt"
	"time"
)

// processStartedAfter is unsupported off Linux: there is no /proc/<pid>/stat
// to read a process start time from, and on those platforms runningImagePath
// itself is unsupported, so this fallback is never reached. The stub keeps the
// cross-platform build working and fails closed (never claims "started after").
func processStartedAfter(pid int, swapTime time.Time) (bool, error) {
	return false, fmt.Errorf("processStartedAfter: unsupported platform")
}
