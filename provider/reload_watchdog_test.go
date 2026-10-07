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
// restores them on cleanup. All three call sites below depend on this so the
// tests run in milliseconds instead of minutes.
func shrinkReloadVars(t *testing.T, timeout, hardLimit, interval, reFire time.Duration) {
	t.Helper()
	oldTimeout, oldLimit := reloadSlotTimeout, reloadHardLimit
	oldInterval, oldReFire := reloadWatchdogInterval, reloadWatchdogReFire
	oldPoll := reloadSlotPollInterval
	reloadSlotTimeout, reloadHardLimit = timeout, hardLimit
	reloadWatchdogInterval, reloadWatchdogReFire = interval, reFire
	reloadSlotPollInterval = 2 * time.Millisecond
	t.Cleanup(func() {
		reloadSlotTimeout, reloadHardLimit = oldTimeout, oldLimit
		reloadWatchdogInterval, reloadWatchdogReFire = oldInterval, oldReFire
		reloadSlotPollInterval = oldPoll
	})
}

// waitWatchdogActionIdle blocks until no stubbed escalation goroutine is in
// flight, so cleanup never restores reloadOverdueAction while a spawned
// action is still about to read it.
func waitWatchdogActionIdle(t *testing.T, r *ProxyReloader) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.watchdogActionInFlight.Load() {
		if time.Now().After(deadline) {
			t.Fatal("escalation goroutine did not finish in time")
		}
		time.Sleep(time.Millisecond)
	}
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
	waitWatchdogActionIdle(t, r)
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
	restoreTrigger := getHotSwapTrigger()
	setHotSwapTrigger(func() error { called.Add(1); return nil })
	t.Cleanup(func() { setHotSwapTrigger(restoreTrigger) })

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

	// A normal release still removes our own lock (the replacement is
	// stale — dead pid and an old timestamp — so acquisition steals it
	// first).
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
	if r.reloadStartedAt.Load() != 0 {
		t.Fatal("an aborted reload must not leave reloadStartedAt set")
	}
}

// A completed reload must leave no watchdog state behind: a stale
// reloadStartedAt from an earlier reload is exactly what let a fresh reload
// look instantly overdue and trip a false escalation.
func TestReloadClearsWatchdogStateAfterCompletion(t *testing.T) {
	r, _, _ := trimFixture(t)
	out := captureTlog(t, func() { r.reload() })
	if strings.Contains(out, "reload aborted") {
		t.Fatalf("fixture reload should complete, got:\n%s", out)
	}
	if r.reloadActive.Load() {
		t.Fatal("a completed reload must clear reloadActive")
	}
	if r.reloadStartedAt.Load() != 0 {
		t.Fatalf("a completed reload must clear reloadStartedAt, got %d", r.reloadStartedAt.Load())
	}
	if !r.mu.TryLock() {
		t.Fatal("a completed reload must release the slot")
	}
	r.mu.Unlock()
}

// A stale or missing timestamp with the active flag set must never escalate:
// the watchdog waits for a real timestamp, and the store order guarantees
// active=true always carries this reload's timestamp.
func TestReloadWatchdogIgnoresStaleOrMissingTimestamp(t *testing.T) {
	// A LONG hard limit: "fresh" must stay fresh for the whole test, since
	// the point is the timestamp guards, not the overdue path.
	shrinkReloadVars(t, time.Minute, time.Minute, 5*time.Millisecond, time.Millisecond)
	withTempHome(t)

	oldAction := reloadOverdueAction
	var calls atomic.Int32
	reloadOverdueAction = func() error { calls.Add(1); return nil }
	t.Cleanup(func() { reloadOverdueAction = oldAction })

	r := &ProxyReloader{}
	// Active with NO timestamp — the window the old publish order exposed:
	// must not fire.
	r.reloadActive.Store(true)
	r.reloadStartedAt.Store(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunReloadWatchdog(ctx)
	}()
	time.Sleep(60 * time.Millisecond)
	// A fresh reload starts: recent timestamp, still no fire.
	r.reloadStartedAt.Store(time.Now().UnixNano())
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if calls.Load() != 0 {
		t.Fatalf("watchdog escalated %d times on missing/fresh reload state", calls.Load())
	}
}

// The re-fire throttle must space escalation attempts, not fire on every
// watchdog tick.
func TestReloadWatchdogRefireIsThrottled(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 10*time.Millisecond, 5*time.Millisecond, 60*time.Millisecond)
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
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	waitWatchdogActionIdle(t, r)
	// Ticks land every 5ms; without the re-fire throttle this would be ~30.
	if calls.Load() < 2 || calls.Load() > 4 {
		t.Fatalf("escalation calls = %d, want 2..4 with a 60ms re-fire over 150ms", calls.Load())
	}
}

// With hot restart allowed but no trigger registered, the action must report
// that cleanly (and never crash on a nil trigger).
func TestReloadOverdueActionWithoutTrigger(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	restoreTrigger := getHotSwapTrigger()
	setHotSwapTrigger(nil)
	t.Cleanup(func() { setHotSwapTrigger(restoreTrigger) })

	t.Setenv("URNETWORK_HOT_RESTART", "")
	err := reloadOverdueAction()
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("want trigger-not-installed error, got %v", err)
	}
}

// Shutdown must cancel a slot wait promptly instead of waiting out the slot
// timeout.
func TestAcquireReloadSlotCancelsOnShutdown(t *testing.T) {
	shrinkReloadVars(t, time.Hour, time.Minute, time.Second, time.Second)
	oldPoll := reloadSlotPollInterval
	reloadSlotPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { reloadSlotPollInterval = oldPoll })

	ctx, cancel := context.WithCancel(context.Background())
	r := &ProxyReloader{parentCtx: ctx}
	r.mu.Lock()
	defer r.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if r.acquireReloadSlot() {
		t.Fatal("must not acquire while the slot is held")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slot wait returned after %v; cancellation must end it promptly", elapsed)
	}
}

// Same-second re-acquisition: identical pid+timestamp but a different nonce
// must not let an earlier release remove the later holder's lock.
func TestProxyLockReleaseSameSecondReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.lock")

	rel, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	own, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(own)), "\n")
	if len(lines) < 3 {
		t.Fatalf("lock content should carry the nonce line, got %q", own)
	}
	// Same pid, same second, different nonce: the same-second collision the
	// token must distinguish.
	replacement := lines[0] + "\n" + lines[1] + "\n" + "18446744073709551615\n"
	if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	rel()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != replacement {
		t.Fatalf("release removed a same-second replacement holder's lock: content %q err %v", got, err)
	}
}

// A new overdue episode must fire immediately even when the previous one
// fired recently: the throttle is per-episode, not global.
func TestReloadWatchdogRefiresForANewEpisode(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 10*time.Millisecond, 5*time.Millisecond, time.Hour)
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
	if calls.Load() == 0 {
		t.Fatal("first episode never fired")
	}
	// End the episode: with reFire = 1h, only an episode reset can fire again.
	r.reloadActive.Store(false)
	time.Sleep(30 * time.Millisecond)
	r.reloadStartedAt.Store(time.Now().Add(-time.Hour).UnixNano())
	r.reloadActive.Store(true)
	deadline = time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	waitWatchdogActionIdle(t, r)
	if calls.Load() < 2 {
		t.Fatalf("a fresh overdue episode must fire despite the 1h re-fire throttle; calls=%d", calls.Load())
	}
}

// A failing escalation is recorded durably and does not stop the watchdog.
func TestReloadWatchdogReportsFailedEscalation(t *testing.T) {
	shrinkReloadVars(t, time.Minute, 10*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	home := withTempHome(t)

	oldAction := reloadOverdueAction
	var calls atomic.Int32
	reloadOverdueAction = func() error { calls.Add(1); return errors.New("boom") }
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
	waitWatchdogActionIdle(t, r)
	if calls.Load() == 0 {
		t.Fatal("watchdog never escalated")
	}
	events, err := os.ReadFile(filepath.Join(home, ".urnetwork", "events.log"))
	if err != nil || !strings.Contains(string(events), "hot restart unavailable: boom") {
		t.Fatalf("failed escalation must be recorded in events.log: err=%v content=%.400s", err, events)
	}
}

// Releasing after the lock file was deleted (any cause) must be a quiet
// no-op.
func TestProxyLockReleaseAfterDeletionIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.lock")
	rel, err := acquireProxyLockAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release after deletion must not recreate the lock, stat err = %v", err)
	}
}
