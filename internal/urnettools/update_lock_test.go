package urnettools

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Two concurrent holders of the same lock must serialize. flock(2) is held
// by the open file description, not the process, so two separate opens in
// one process contend exactly as two processes do. That is what makes this
// testable in-process.
func TestAcquireExclusiveLockSerializes(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "update.lock")

	first, err := acquireExclusiveLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, err := acquireExclusiveLock(lockPath)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("second holder acquired the lock while the first still held it; concurrent updates would interleave their backup and swap")
	case <-time.After(250 * time.Millisecond):
		// Correct: still blocked.
	}

	first()

	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never acquired the lock after release; an update would hang instead of waiting its turn")
	}
	wg.Wait()
}

// The lock must be reusable back to back, so a timer firing every week does
// not accumulate anything or wedge on its own previous run.
func TestAcquireExclusiveLockIsReusable(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "update.lock")
	for i := 0; i < 3; i++ {
		release, err := acquireExclusiveLock(lockPath)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		release()
	}
}

// A lock path whose directory does not exist must report a clear error
// rather than panic, since p.Binary is operator-supplied by way of unit
// discovery.
func TestAcquireExclusiveLockMissingDirErrors(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "no-such-dir", "update.lock")
	release, err := acquireExclusiveLock(lockPath)
	if err == nil {
		release()
		t.Fatal("expected an error for a lock path in a missing directory")
	}
}

// The pending-overrides lock still works through the generalized helper,
// and still uses the same ".lock" suffix on the queue file so an older
// urnet-tools sharing the box contends on the same path.
func TestPendingOverridesLockStillUsesQueueFileSuffix(t *testing.T) {
	dir := t.TempDir()
	queueFile := filepath.Join(dir, "pending_overrides.json")

	release, err := acquirePendingOverridesLock(queueFile)
	if err != nil {
		t.Fatalf("acquire pending-overrides lock: %v", err)
	}
	defer release()

	if _, err := os.Stat(queueFile + ".lock"); err != nil {
		t.Fatalf("expected the lock file at %s.lock: %v", queueFile, err)
	}
}
