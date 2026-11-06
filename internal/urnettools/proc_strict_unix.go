//go:build !windows

package urnettools

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// processAliveStrict is processAlive with the opposite bias on doubt: only
// ESRCH (no such process) and a zombie/dead state prove the process is gone.
// EPERM and every other error mean it exists or cannot be inspected, so it
// counts as alive and the caller must not delete files under it.
func processAliveStrict(pid int) bool {
	if pid <= 0 {
		return false
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 && (fields[2] == "Z" || fields[2] == "X") {
			return false
		}
	}
	return syscall.Kill(pid, 0) != syscall.ESRCH
}
