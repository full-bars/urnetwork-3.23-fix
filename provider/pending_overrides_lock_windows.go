//go:build windows

package main

import (
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// On Windows there is no flock(2). acquirePendingOverridesLock mirrors
// internal/urnettools/pending_overrides_lock_windows.go using LockFileEx
// (blocking, exclusive) on the same pending_overrides.json.lock so the
// provider's startup read-apply-delete and urnet-tools' append are mutually
// excluded across processes. pendingOverridesLockMu stays held for the
// caller's entire critical section, ordering goroutines within this process;
// LockFileEx is what excludes the other process.
var pendingOverridesLockMu sync.Mutex

func acquirePendingOverridesLock(queueFile string) (func(), error) {
	pendingOverridesLockMu.Lock()

	lockPath := queueFile + ".lock"
	pathPtr, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		pendingOverridesLockMu.Unlock()
		return nil, fmt.Errorf("pending-overrides lock path: %w", err)
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
		pendingOverridesLockMu.Unlock()
		return nil, fmt.Errorf("open pending-overrides lock file: %w", err)
	}

	// Block (no LOCKFILE_FAIL_IMMEDIATELY) until the exclusive byte-range
	// lock is available, same blocking contract as unix.Flock(LOCK_EX).
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		windows.CloseHandle(handle)
		pendingOverridesLockMu.Unlock()
		return nil, fmt.Errorf("lock pending-overrides: %w", err)
	}

	return func() {
		var unlockOverlapped windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 1, 0, &unlockOverlapped)
		windows.CloseHandle(handle)
		pendingOverridesLockMu.Unlock()
	}, nil
}
