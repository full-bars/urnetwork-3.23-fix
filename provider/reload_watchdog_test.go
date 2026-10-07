package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkReloadVars swaps the reload timing vars for test-sized values and
// restores them on cleanup. The tests below depend on this so they run in
// milliseconds instead of minutes.
func shrinkReloadVars(t *testing.T, timeout, hardLimit, interval, reFire time.Duration) {
	t.Helper()
	oldTimeout, oldLimit := reloadSlotTimeout, reloadHardLimit
	oldInterval, oldReFire := reloadWatchdogInterval, reloadWatchdogReFire
	reloadSlotTimeout, reloadHardLimit = timeout, hardLimit
	reloadWatchdogInterval, reloadWatchdogReFire = interval, reFire
	t.Cleanup(func() {
		reloadSlotTimeout, reloadHardLimit = oldTimeout, oldLimit
		reloadWatchdogInterval, reloadWatchdogReFire = oldInterval, oldReFire
	})
}

// A reload that cannot take the slot must SKIP — with a loud line naming how
// long the previous reload has held it — instead of blocking forever on the
// mutex the way it did before (the LA7 wedge: one stuck reload silently
// stopped every future trigger for 13h).
func TestReloadSkipsWhenSlotHeldBeyondTimeout(t *testing.T) {
	shrinkReloadVars(t, 30*time.Millisecond, time.Minute, time.Second, time.Second)
	withTempHome(t)

	r := &ProxyReloader{}
	r.reloadStartedAt.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	r.mu.Lock() // simulate a reload stuck in-flight
	defer r.mu.Unlock()

	out := captureTlog(t, func() { r.reload() })
	if !strings.Contains(out, "previous reload has held the slot") {
		t.Fatalf("expected slot-skip log, got:\n%s", out)
	}
	if !strings.Contains(out, "5m0s") {
		t.Fatalf("expected the held duration in the log, got:\n%s", out)
	}
}

// When the in-flight reload finishes inside the wait window, the new reload
// proceeds instead of skipping: the slot wait must still wait when it can.
func TestAcquireReloadSlotWaitsForInFlightReload(t *testing.T) {
	shrinkReloadVars(t, 2*time.Second, time.Minute, time.Second, time.Second)
	oldPoll := reloadSlotPollInterval
	reloadSlotPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { reloadSlotPollInterval = oldPoll })

	r := &ProxyReloader{}
	r.mu.Lock()
	go func() {
		time.Sleep(25 * time.Millisecond)
		r.mu.Unlock()
	}()

	start := time.Now()
	if !r.acquireReloadSlot() {
		t.Fatal("slot must be acquired once the in-flight reload finishes within the wait window")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("acquire returned after %v; it must have waited for the in-flight release", elapsed)
	}
	r.mu.Unlock()
}

// An overdue reload must trigger the escalation (hot restart request) through
// the reloadOverdueAction hook, and a quiet reloader must not.
func TestReloadWatchdogEscalatesOnOverdueReload(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 10*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	withTempHome(t)

	oldAction := reloadOverdueAction
	var calls atomic.Int32
	reloadOverdueAction = func() error { calls.Add(1); return nil }
	t.Cleanup(func() { reloadOverdueAction = oldAction })

	r := &ProxyReloader{}
	r.reloadActive.Store(true)
	r.reloadStartedAt.Store(time.Now().Add(-time.Hour).UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunReloadWatchdog(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() < 1 {
		t.Fatal("watchdog never escalated for a reload overdue past the hard limit")
	}
}

func TestReloadWatchdogQuietWhenNoReloadActive(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 10*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	withTempHome(t)

	oldAction := reloadOverdueAction
	var calls atomic.Int32
	reloadOverdueAction = func() error { calls.Add(1); return nil }
	t.Cleanup(func() { reloadOverdueAction = oldAction })

	r := &ProxyReloader{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunReloadWatchdog(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if calls.Load() != 0 {
		t.Fatalf("watchdog escalated %d times with no active reload", calls.Load())
	}
}

// The default escalation honors the hot_restart kill switch and calls the
// registered hot-swap trigger when allowed.
func TestReloadOverdueActionHonorsHotRestartSwitch(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	var called atomic.Int32
	restoreTrigger := hotSwapTrigger
	hotSwapTrigger = func() error { called.Add(1); return nil }
	t.Cleanup(func() { hotSwapTrigger = restoreTrigger })

	t.Setenv("URNETWORK_HOT_RESTART", "0")
	err := reloadOverdueAction()
	if err == nil || !strings.Contains(err.Error(), "hot restart disabled") {
		t.Fatalf("want disabled-switch error, got %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("trigger must not fire while hot restart is off")
	}

	t.Setenv("URNETWORK_HOT_RESTART", "")
	if err := reloadOverdueAction(); err != nil {
		t.Fatalf("want the registered trigger to run, got %v", err)
	}
	if called.Load() != 1 {
		t.Fatalf("trigger calls = %d, want 1", called.Load())
	}
}

// A release must remove the lock ONLY while the file still carries this
// holder's pid+timestamp. A late release used to remove whatever lock file
// was present, including a replacement holder's, admitting a third acquirer
// while the replacement was still working.
func TestProxyLockReleaseKeepsReplacementLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.lock")

	rel, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a stale steal replacing our lock with another holder's.
	replacement := "424242\n1700000000\n"
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	rel()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != replacement {
		t.Fatalf("release touched a replacement holder's lock: content %q err %v", got, err)
	}

	// A normal release still removes our own lock (the aged replacement is
	// stale, so acquisition steals it first).
	rel2, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("own lock must be removed on release, stat err = %v", err)
	}
}

// A reload that runs past the hard limit aborts at its next phase boundary,
// releases the slot, and leaves nothing half-applied before the mutation
// section.
func TestReloadAbortsAtHardLimit(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 0, time.Second, time.Second)
	withTempHome(t)

	r := &ProxyReloader{}
	out := captureTlog(t, func() { r.reload() })
	if !strings.Contains(out, "reload aborted at start") {
		t.Fatalf("expected hard-limit abort line, got:\n%s", out)
	}
	if !r.mu.TryLock() {
		t.Fatal("the aborted reload must release the slot")
	}
	r.mu.Unlock()
	if r.reloadActive.Load() {
		t.Fatal("an aborted reload must not leave reloadActive set")
	}
}
