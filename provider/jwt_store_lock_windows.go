//go:build windows

package main

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// On Windows there is no flock(2), so acquireJWTStoreLock uses LockFileEx
// (blocking, exclusive) on a dedicated lock file for the real inter-process
// mutex, mirroring jwt_store_lock_unix.go's unix.Flock(LOCK_EX). jwtLockMu
// stays held for the caller's entire critical section (until release()
// runs), matching that same contract on Unix — it only orders goroutines
// within this process; LockFileEx is what excludes a concurrent process
// (e.g. a HotSwap parent/candidate pair, each with its own clientJWTStore).
var jwtLockMu sync.Mutex

const jwtLockSuffix = ".lock"

func acquireJWTStoreLock(path string) (func(), error) {
	jwtLockMu.Lock()

	lockPath := path + jwtLockSuffix
	pathPtr, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		jwtLockMu.Unlock()
		return nil, fmt.Errorf("jwt lock path: %w", err)
	}

	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		jwtLockMu.Unlock()
		return nil, fmt.Errorf("open jwt lock file: %w", err)
	}

	// Block (no LOCKFILE_FAIL_IMMEDIATELY) until the exclusive byte-range
	// lock is available, same blocking contract as unix.Flock(LOCK_EX).
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		windows.CloseHandle(handle)
		jwtLockMu.Unlock()
		return nil, fmt.Errorf("lock jwt store: %w", err)
	}

	return func() {
		var unlockOverlapped windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 1, 0, &unlockOverlapped)
		windows.CloseHandle(handle)
		// Do NOT os.Remove(lockPath): no other lock in the codebase removes
		// its lock file (unix jwt lock, both pending-overrides locks keep it),
		// and removing a lock file that another process has open in LockFileEx
		// can break that waiter's lock on a deleted file. The lock file's
		// presence on disk is harmless.
		jwtLockMu.Unlock()
	}, nil
}
