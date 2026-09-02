//go:build !linux && !windows

package urnettools

import "fmt"

// runningImagePath returns the filesystem path of the image the process
// identified by pid is actually executing. On platforms without /proc and
// without a implemented resolver (currently everything except Linux and
// Windows) this reports an unsupported error; callers treat that as
// "cannot determine currency" and must NOT fall back to the on-disk
// binary, which after an update swap is the NEW image regardless of what
// the process runs.
func runningImagePath(pid int) (string, error) {
	return "", fmt.Errorf("runningImagePath: unsupported platform (pid %d)", pid)
}
