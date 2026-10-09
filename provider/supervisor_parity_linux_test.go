//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// T1b: Pin the thrash watchdog constants (Linux only, like the watchdog).
func TestSupervisorParityThrashConstants(t *testing.T) {
	durationChecks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"thrashSampleInterval", thrashSampleInterval, 30 * time.Second},
		{"thrashSustain", thrashSustain, 3 * time.Minute},
		{"thrashSevereSustain", thrashSevereSustain, 90 * time.Second},
		{"thrashCalmRelax", thrashCalmRelax, 5 * time.Minute},
		{"thrashCriticalAfter", thrashCriticalAfter, 15 * time.Minute},
		{"thrashRetryInterval", thrashRetryInterval, 10 * time.Minute},
		{"thrashRecoveryCheckAfter", thrashRecoveryCheckAfter, 5 * time.Minute},
		{"thrashPersistTimeout", thrashPersistTimeout, 5 * time.Second},
		{"thrashLedgerTimeout", thrashLedgerTimeout, 2 * time.Second},
		{"thrashPriorEscalationWindow", thrashPriorEscalationWindow, 30 * time.Minute},
	}
	for _, tc := range durationChecks {
		if tc.got != tc.want {
			t.Errorf("supervisor parity: %s changed; if intentional update docs/design/self-healing-supervisor.md section 4.4", tc.name)
		}
	}

	floatChecks := []struct {
		name string
		got  float64
		want float64
	}{
		{"thrashSevereFrac", thrashSevereFrac, 0.25},
		{"thrashMildFrac", thrashMildFrac, 0.10},
	}
	for _, tc := range floatChecks {
		if tc.got != tc.want {
			t.Errorf("supervisor parity: %s changed; if intentional update docs/design/self-healing-supervisor.md section 4.4", tc.name)
		}
	}

	if thrashExitCode != 75 {
		t.Errorf("supervisor parity: thrashExitCode changed; if intentional update docs/design/self-healing-supervisor.md section 4.4")
	}
}

// T2: Test all branches of thrashHotSwapBusy(), including the lock-held branch.
func TestThrashHotSwapBusyBranches(t *testing.T) {
	origDraining := isHotSwapDraining.Load()
	t.Cleanup(func() {
		isHotSwapDraining.Store(origDraining)
	})

	// 1. Not draining and hotSwapLock is free -> false
	isHotSwapDraining.Store(false)
	if thrashHotSwapBusy() {
		t.Fatalf("thrashHotSwapBusy() = true when not draining and hotSwapLock free, want false")
	}

	// 2. When isHotSwapDraining is set -> true
	isHotSwapDraining.Store(true)
	if !thrashHotSwapBusy() {
		t.Fatalf("thrashHotSwapBusy() = false when isHotSwapDraining set, want true")
	}
	isHotSwapDraining.Store(false)
	if thrashHotSwapBusy() {
		t.Fatalf("thrashHotSwapBusy() = true after resetting isHotSwapDraining, want false")
	}

	// 3. While hotSwapLock is held (TryLock fails) -> true
	hotSwapLock.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			hotSwapLock.Unlock()
		}
	})
	if !thrashHotSwapBusy() {
		t.Fatalf("thrashHotSwapBusy() = false while hotSwapLock held, want true")
	}

	// 4. False again after unlock
	hotSwapLock.Unlock()
	locked = false
	if thrashHotSwapBusy() {
		t.Fatalf("thrashHotSwapBusy() = true after hotSwapLock unlocked, want false")
	}
}

// T4: A characterization test that pins today's behavior:
// reloadOverdueAction calls the installed hot-swap trigger when hot restart is enabled,
// without consulting thrash state.
func TestReloadOverdueActionIgnoresThrashStateToday(t *testing.T) {
	// Pins current behavior. The supervisor design (rung e thrash veto) will flip this deliberately; update this test in the same change.
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)

	t.Setenv("URNETWORK_HOT_RESTART", "")

	// Put the thrash system in a thrashing state using existing seams:
	// globalThrashSnap and thrashFreeze.
	snap := &thrashSnapshot{
		State:   "thrashing",
		Summary: "memory thrash in progress",
	}
	globalThrashSnap.Store(snap)
	t.Cleanup(func() { globalThrashSnap.Store(nil) })

	thrashFreeze.Store(true)
	t.Cleanup(func() { thrashFreeze.Store(false) })

	// Verify thrash state is recognized by thrashStateName.
	if thrashStateName() != "thrashing" {
		t.Fatalf("thrashStateName() = %q, want thrashing", thrashStateName())
	}

	// Install a mock hot-swap trigger.
	var triggerCalls atomic.Int32
	restoreTrigger := getHotSwapTrigger()
	setHotSwapTrigger(func() error {
		triggerCalls.Add(1)
		return nil
	})
	t.Cleanup(func() { setHotSwapTrigger(restoreTrigger) })

	// reloadOverdueAction ignores thrash state today and calls the trigger.
	err := reloadOverdueAction()
	if err != nil {
		t.Fatalf("reloadOverdueAction() returned error: %v, want nil", err)
	}
	if triggerCalls.Load() != 1 {
		t.Fatalf("trigger calls = %d, want 1", triggerCalls.Load())
	}
}

// T5: With no container opt-in, the exit-75 escape is inert unless INVOCATION_ID
// or NOTIFY_SOCKET is set. (A container that sets URNETWORK_EXIT75_OK=1 is a
// supervisor too; that path is covered by the thrash watchdog tests.) Uses
// t.Setenv and the thrashExitFn seam to assert:
// neither set means thrashExitFn is not called;
// either set means the path proceeds to call it with thrashExitCode.
// The host must not leak in: a Docker CI runner has /.dockerenv, so the
// container probe is stubbed out along with its environment switches.
func TestThrashExitGateNeedsInitSystem(t *testing.T) {
	setupWatchdogEnv := func(t *testing.T, invocationID, notifySocket string) (exited func() bool, code func() int, ticks func() int) {
		home := withTempHome(t)
		t.Setenv("INVOCATION_ID", invocationID)
		t.Setenv("NOTIFY_SOCKET", notifySocket)
		t.Setenv("URNETWORK_CONTAINER", "")
		t.Setenv("URNETWORK_EXIT75_OK", "")
		prevFileExists := thrashFileExistsFn
		thrashFileExistsFn = func(string) bool { return false }
		t.Cleanup(func() { thrashFileExistsFn = prevFileExists })

		if err := globalControlState.set("proxy_self_heal", "on"); err == nil {
			t.Cleanup(func() { _ = globalControlState.clear("proxy_self_heal") })
		}
		if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(home, ".urnetwork", "proxy_self_heal"), []byte("on\n"), 0o600); err != nil {
			t.Fatalf("write override: %v", err)
		}
		ResetHotSwapStateForTest()
		t.Cleanup(ResetHotSwapStateForTest)

		restore := shrinkThrashDurations()
		t.Cleanup(restore)

		var calls atomic.Int32
		var psiFull, swapIn uint64
		prevRead := thrashReadFn
		thrashReadFn = func() thrashRead {
			if calls.Add(1) == 1 {
				return thrashRead{psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true, swapOK: true, swapUnit: true, ramAvailOK: true, ramAvailMiB: 500}
			}
			psiFull += 50_000_000
			swapIn += 100_000
			return thrashRead{
				psiSomeTotal: psiFull, psiFullTotal: psiFull,
				psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true,
				swapIn: swapIn, swapOut: swapIn, swapOK: true, swapUnit: true,
				unitSwapMiB: 3000, unitSwapOK: true,
				hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300, hostSwapOK: true,
				heapFrac: 3.1, heapUsedMiB: 2100, heapLimitMiB: 680, heapOK: true,
				ramAvailMiB: 82, ramAvailOK: true,
			}
		}
		t.Cleanup(func() { thrashReadFn = prevRead })

		var exitCalled atomic.Bool
		var exitCode atomic.Int32
		exitCode.Store(-1)
		prevExit := thrashExitFn
		thrashExitFn = func(code int) {
			exitCode.Store(int32(code))
			exitCalled.Store(true)
		}
		t.Cleanup(func() { thrashExitFn = prevExit })

		return func() bool { return exitCalled.Load() }, func() int { return int(exitCode.Load()) }, func() int { return int(calls.Load()) }
	}

	// runUntilExit runs the watchdog and returns when it exits by itself. On a
	// timeout it cancels the loop and waits for it to stop before failing, so a
	// stray tick cannot run against the globals the cleanup restores.
	runUntilExit := func(t *testing.T) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() {
			runThrashWatchdog(ctx, true)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			cancel()
			<-done
			t.Fatal("watchdog timed out waiting for escalation")
		}
	}

	t.Run("neither set leaves exitFn uncalled", func(t *testing.T) {
		calledFn, _, ticksFn := setupWatchdogEnv(t, "", "")

		// Direct check on thrashEscalate logic:
		rt := thrashRates{fullFrac: 0.5, fullOK: true, swapOK: true, swapInPS: 500, swapOutPS: 500}
		rdUnit := thrashRead{unitSwapOK: true, unitSwapMiB: 3000, hostSwapOK: true, hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300}
		now := time.Now()
		esc := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
		if esc.Code != "no-supervisor" || esc.Action != "alert" {
			t.Fatalf("thrashEscalate with neither set got (%s, %s), want (alert, no-supervisor)", esc.Action, esc.Code)
		}

		// Watchdog execution: loop ticks repeatedly through severe thrash condition,
		// but exitFn is never called because no supervisor is present.
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		runThrashWatchdog(ctx, true)

		if calledFn() {
			t.Fatalf("thrashExitFn was called when neither INVOCATION_ID nor NOTIFY_SOCKET set")
		}
		// the loop really ticked through the thrash condition; a stalled loop
		// under CPU load would otherwise pass this subtest without testing the gate
		if got := ticksFn(); got < 3 {
			t.Fatalf("the watchdog read the sensors %d times in 80ms; the gate was not exercised", got)
		}
	})

	t.Run("INVOCATION_ID set calls exitFn with thrashExitCode", func(t *testing.T) {
		calledFn, codeFn, _ := setupWatchdogEnv(t, "test-supervisor-inv", "")

		runUntilExit(t)

		if !calledFn() {
			t.Fatalf("thrashExitFn was not called when INVOCATION_ID was set")
		}
		if codeFn() != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", codeFn(), thrashExitCode)
		}
	})

	t.Run("NOTIFY_SOCKET set calls exitFn with thrashExitCode", func(t *testing.T) {
		calledFn, codeFn, _ := setupWatchdogEnv(t, "", "/run/systemd/notify/test.sock")

		runUntilExit(t)

		if !calledFn() {
			t.Fatalf("thrashExitFn was not called when NOTIFY_SOCKET was set")
		}
		if codeFn() != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", codeFn(), thrashExitCode)
		}
	})
}
