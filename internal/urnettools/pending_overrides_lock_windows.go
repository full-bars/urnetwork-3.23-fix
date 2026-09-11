//go:build windows

package urnettools

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process
// lock on stateDir's pending_overrides.json so two concurrent `urnet-tools`
// invocations can't each read the same queue, append their own op, and let
// the last os.Rename in queuePendingOverride silently discard the other's
// update. Mirrors pending_overrides_lock_unix.go's unix.Flock(LOCK_EX)
// using LockFileEx, since Windows has no flock(2).
func acquirePendingOverridesLock(queueFile string) (func(), error) {
	release, err := acquireExclusiveLock(queueFile + ".lock")
	if err != nil {
		return nil, fmt.Errorf("pending-overrides: %w", err)
	}
	return release, nil
}

// acquireExclusiveLock obtains a blocking, exclusive inter-process lock on
// lockPath and returns a release function. Mirrors the unix build's
// unix.Flock(LOCK_EX) using LockFileEx, since Windows has no flock(2).
// Windows releases the lock when the handle closes, including on process
// death, so a crash cannot leave a stale lock behind.
func acquireExclusiveLock(lockPath string) (func(), error) {
	pathPtr, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock path %s: %w", lockPath, err)
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
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}

	return func() {
		var unlockOverlapped windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 1, 0, &unlockOverlapped)
		windows.CloseHandle(handle)
	}, nil
}
