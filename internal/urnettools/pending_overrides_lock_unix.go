//go:build !windows

package urnettools

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process
// lock on stateDir's pending_overrides.json so two concurrent `urnet-tools`
// invocations (e.g. two operators, or a script looping `set` calls) can't
// each read the same queue, append their own op, and let the last
// os.Rename in queuePendingOverride silently discard the other's update.
// Returns a release function that must be called when done.
func acquirePendingOverridesLock(queueFile string) (func(), error) {
	release, err := acquireExclusiveLock(queueFile + ".lock")
	if err != nil {
		return nil, fmt.Errorf("pending-overrides: %w", err)
	}
	return release, nil
}

// acquireExclusiveLock obtains a blocking, exclusive inter-process lock on
// lockPath and returns a release function. Blocking rather than try-lock on
// purpose: a caller that loses the race should wait its turn, not fail.
//
// flock(2) is held by the open file description and the kernel drops it when
// the process exits, however it exits, so a crash cannot leave a stale lock
// that wedges the fleet. That is the reason for flock over a pidfile.
func acquireExclusiveLock(lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
