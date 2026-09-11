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

// runningImageHandle returns a path that reads and execs the image pid is
// running, even after the on-disk binary has been swapped. It returns the
// /proc/<pid>/exe magic symlink itself rather than its target: the target
// string is a plain path that stops resolving the moment an update renames
// over the binary (the kernel then reports it with a " (deleted)" suffix),
// while the symlink keeps pointing at the loaded inode for the life of the
// process.
//
// Prefer this over runningImagePath wherever the goal is to READ the running
// image (version, ELF header) rather than to DISPLAY or classify its path.
// Reading through it is also safer than reading the on-disk path: it names
// the inode already executing, which a local user cannot substitute, whereas
// a filesystem path can be replaced between the check and the read.
func runningImageHandle(pid int) (string, error) {
	// Readlink first: it fails for a nonexistent pid and for a process this
	// user may not inspect, so a success proves the handle is usable before
	// any caller reads or execs it.
	if _, err := runningImagePath(pid); err != nil {
		return "", err
	}
	return fmt.Sprintf("/proc/%d/exe", pid), nil
}
