//go:build !windows

package urnettools

import "syscall"

// lockShared acquires a shared (read) file lock. Unix uses flock(2).
func lockShared(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_SH)
}

// lockExclusive acquires an exclusive (write) file lock. Unix uses flock(2).
func lockExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX)
}

// unlock releases a file lock.
func unlock(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}
