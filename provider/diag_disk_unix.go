//go:build unix

package main

import "syscall"

// diskFreeBytes is the space an unprivileged writer can use on the filesystem
// holding dir (Bavail, not Bfree: the root-reserved blocks are not ours).
func diskFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
