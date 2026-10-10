//go:build linux

package main

import (
	"context"
	"testing"
	"time"
)

// openRcLivenessRig points the supervisor seams at OpenRC, drives the tick fast
// and records any exit. The progress clock is left well past the stall budget,
// so the loop sees a provider that is alive but making no progress.
func openRcLivenessRig(t *testing.T) (exited chan int) {
	t.Helper()
	withTempHome(t)
	prevEnv, prevFiles, prevExit, prevTick := thrashEnvFn, thrashFileExistsFn, thrashExitFn, livenessOpenRcTick
	t.Cleanup(func() {
		thrashEnvFn, thrashFileExistsFn, thrashExitFn, livenessOpenRcTick = prevEnv, prevFiles, prevExit, prevTick
	})
	thrashEnvFn = func(k string) string {
		if k == "URNETWORK_INIT" {
			return "openrc"
		}
		return ""
	}
	thrashFileExistsFn = func(string) bool { return false }
	livenessOpenRcTick = 5 * time.Millisecond

	exited = make(chan int, 1)
	thrashExitFn = func(c int) { exited <- c }

	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	processLiveness.nano.Store(time.Now().Add(-time.Hour).UnixNano())
	processLiveness.ticked.Store(true)
	t.Cleanup(func() {
		processLiveness.ticked.Store(false)
		processLiveness.note()
	})
	return exited
}

// TestOpenRcLivenessExitsOnStall: supervise-daemon has no watchdog of its own,
// so a provider that is alive but making no progress must ask for its own
// restart by exiting 75, and leave the same lean start cap a thrash escape
// leaves.
func TestOpenRcLivenessExitsOnStall(t *testing.T) {
	exited := openRcLivenessRig(t)
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go runOpenRcLivenessWatchdog(ctx, livenessGate)

	select {
	case code := <-exited:
		if code != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", code, thrashExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled provider under OpenRC must exit so supervise-daemon restarts it")
	}
	if cap, ok := activeThrashCap(time.Now()); !ok || cap != 300 {
		t.Fatalf("the exit must leave the lean start cap (300 of 500), got %d ok=%v", cap, ok)
	}
}

// TestOpenRcLivenessRespectsTheSelfHealSwitch: with self-heal off the provider
// keeps running and leaves no restart entry behind.
func TestOpenRcLivenessRespectsTheSelfHealSwitch(t *testing.T) {
	exited := openRcLivenessRig(t)
	t.Setenv("URNETWORK_SELF_HEAL", "")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runOpenRcLivenessWatchdog(ctx, livenessGate)
		close(done)
	}()

	select {
	case code := <-exited:
		t.Fatalf("self-heal off must not exit, got %d", code)
	case <-done:
	}
	if len(readThrashCapState().Restarts) != 0 {
		t.Fatal("self-heal off must not leave a restart entry behind")
	}
}

// TestLivenessAppliesOnlyUnderASupervisor pins when the stall defense runs at
// all: with a systemd watchdog there is a ping to withhold; under OpenRC's
// supervise-daemon the action is a self-exit; with neither there is no action to
// take, so the loop must not run.
func TestLivenessAppliesOnlyUnderASupervisor(t *testing.T) {
	withTempHome(t)
	prevEnv, prevFiles := thrashEnvFn, thrashFileExistsFn
	t.Cleanup(func() { thrashEnvFn, thrashFileExistsFn = prevEnv, prevFiles })
	thrashEnvFn = func(string) string { return "" }
	thrashFileExistsFn = func(string) bool { return false }

	if livenessApplies() {
		t.Fatal("no supervisor: the stall defense must not run")
	}
	thrashEnvFn = func(k string) string {
		if k == "URNETWORK_INIT" {
			return "openrc"
		}
		return ""
	}
	if !livenessApplies() {
		t.Fatal("under OpenRC the stall defense runs with a self-exit as its action")
	}
	t.Setenv("WATCHDOG_USEC", "1200000000")
	if !livenessApplies() {
		t.Fatal("with WatchdogSec= set the systemd feed runs")
	}
}

// TestRunSdWatchdogDispatchesToOpenRc: with no systemd watchdog the real entry
// point must route to the OpenRC self-exit path rather than returning early, or
// the stall defense would be silently inert on the host it exists for.
func TestRunSdWatchdogDispatchesToOpenRc(t *testing.T) {
	exited := openRcLivenessRig(t)
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	t.Setenv("WATCHDOG_USEC", "") // no systemd watchdog on this host

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go runSdWatchdog(ctx, livenessGate)

	select {
	case code := <-exited:
		if code != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", code, thrashExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSdWatchdog must dispatch to the OpenRC stall path when there is no systemd watchdog")
	}
}
