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
	f, err := os.OpenFile(queueFile+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pending-overrides lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock pending-overrides: %w", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
