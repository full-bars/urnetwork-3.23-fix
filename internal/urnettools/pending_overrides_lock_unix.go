//go:build !windows

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process
// lock on stateDir's pending_overrides.json so two concurrent `urnet-tools`
// invocations (e.g. two operators, or a script looping `set` calls) can't
// each read the same queue, append their own op, and let the last
// os.Rename in queuePendingOverride silently discard the other's update.
// Returns a release function that must be called when done.
func acquirePendingOverridesLock(queueFile string) (func(), error) {
	// A freshly created lock file must belong to the state-dir owner, or the
	// unprivileged provider's own merge cannot open it and queued overrides
	// are silently never applied. The ownership change is made on the OPEN
	// descriptor inside acquireExclusiveLockOwned (best-effort): re-resolving
	// the pathname afterwards would let a local user swap it before root's
	// chown.
	release, err := acquireExclusiveLockOwned(queueFile+".lock", filepath.Dir(queueFile))
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
	return acquireExclusiveLockOwned(lockPath, "")
}

// acquireExclusiveLockOwned is acquireExclusiveLock that also hands the lock
// file to the owner of ownerDir (when non-empty) via fchown on the descriptor
// it just opened. Best-effort: a chown failure is not a lock failure.
func acquireExclusiveLockOwned(lockPath, ownerDir string) (func(), error) {
	// O_NOFOLLOW: a planted symlink at the lock path must not make root
	// create the symlink TARGET (e.g. a planted ...json.lock -> /etc/nologin
	// would block non-root logins once root O_CREATEs it) — H2.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if ownerDir != "" {
		_ = chownFdLikeStateOwner(ownerDir, int(f.Fd()))
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
