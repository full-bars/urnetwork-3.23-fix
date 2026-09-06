//go:build !windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireJWTStoreLock obtains an exclusive inter-process lock on the JWT store
// file so that two live processes (parent + candidate) cannot interleave
// flushLocked writes that would cause stale-map overwrites (F-9).
// Returns a release function that must be called when done.
func acquireJWTStoreLock(path string) (func(), error) {
	f, err := os.OpenFile(path+jwtLockSuffix, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open jwt lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock jwt store: %w", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

const jwtLockSuffix = ".lock"
