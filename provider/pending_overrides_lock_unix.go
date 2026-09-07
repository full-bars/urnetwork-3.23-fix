//go:build !windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process lock
// on the pending_overrides.json queue file — the SAME lock file and semantics
// urnet-tools' queuePendingOverride uses (internal/urnettools/
// pending_overrides_lock_unix.go). The provider reads, applies, and deletes
// the queue at startup (mergePendingOverrides); urnet-tools appends to it when
// the control socket is unreachable. Both processes flock
// pending_overrides.json.lock so the provider's read-then-delete cannot
// interleave with urnet-tools' read-append-rename and silently drop a queued op
// that was written after the provider read the file but before it removed it.
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
