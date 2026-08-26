//go:build !linux

package urnettools

import "fmt"

// recoverDeletedBinary is unsupported off-Linux: /proc/<pid>/exe does not exist
// on macOS or Windows, so a deleted-but-running binary cannot be re-hydrated.
// ensureBinaryRecoverable surfaces the plain-language message naming the remedy
// (restart the unit) instead of the bare fork/exec ENOENT.
func recoverDeletedBinary(binary string, pid int, user string) error {
	return fmt.Errorf("auto-recovery of a deleted-but-running provider binary is not supported on this platform")
}
