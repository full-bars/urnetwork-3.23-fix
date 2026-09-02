//go:build linux

package urnettools

import (
	"fmt"
	"os"
)

// runningImagePath returns the filesystem path of the image the process
// identified by pid is actually executing. On Linux this reads
// /proc/<pid>/exe, which resolves to the loaded image (with a
// " (deleted)" suffix when the on-disk binary has since been swapped).
// Callers must NOT substitute the on-disk binary path: after an update
// swaps the binary, the on-disk file is the NEW image while the running
// process may still execute the OLD one, so reading the on-disk file is
// tautological and proves nothing about restart verification.
func runningImagePath(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("runningImagePath: invalid pid %d", pid)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", fmt.Errorf("runningImagePath: read /proc/%d/exe: %w", pid, err)
	}
	return exe, nil
}
