//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// selfExitLivenessRig points the supervisor seams at a supervisor with no
// watchdog (OpenRC's supervise-daemon, or a container's start script), drives
// the tick fast and records any exit. The progress clock is left well past the
// stall budget, so the loop sees a provider that is alive but making no
// progress.
func selfExitLivenessRig(t *testing.T, env map[string]string) (exited chan int) {
	t.Helper()
	withTempHome(t)
	prevEnv, prevFiles, prevExit, prevTick := thrashEnvFn, thrashFileExistsFn, thrashExitFn, livenessSelfExitTick
	prevWritable := thrashCheckCapDirWritableFn
	t.Cleanup(func() {
		thrashEnvFn, thrashFileExistsFn, thrashExitFn, livenessSelfExitTick = prevEnv, prevFiles, prevExit, prevTick
		thrashCheckCapDirWritableFn = prevWritable
	})
	thrashEnvFn = func(k string) string { return env[k] }
	thrashFileExistsFn = func(string) bool { return false }
	livenessSelfExitTick = 5 * time.Millisecond

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

// waitSelfExit waits for the recorded exit and pins the code.
func waitSelfExit(t *testing.T, exited chan int, what string) {
	t.Helper()
	select {
	case code := <-exited:
		if code != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", code, thrashExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s must exit so its supervisor restarts it", what)
	}
}

// holdsSelfHealOff runs the loop for a short window and asserts no exit and no
// restart entry, the shape every refusal here must have.
func assertNoSelfExit(t *testing.T, exited chan int, run func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	select {
	case code := <-exited:
		t.Fatalf("a refused self-exit must not exit, got %d", code)
	case <-done:
	}
	if len(readThrashCapState().Restarts) != 0 {
		t.Fatal("a refused self-exit must not leave a restart entry behind")
	}
}

// TestSelfExitLivenessExitsOnStallUnderOpenRc: supervise-daemon has no watchdog
// of its own, so a provider that is alive but making no progress must ask for
// its own restart by exiting 75, and leave the same lean start cap a thrash
// escape leaves.
func TestSelfExitLivenessExitsOnStallUnderOpenRc(t *testing.T) {
	exited := selfExitLivenessRig(t, map[string]string{"URNETWORK_INIT": "openrc"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go runSelfExitLivenessWatchdog(ctx, livenessGate)

	waitSelfExit(t, exited, "a stalled provider under OpenRC")
	if cap, ok := activeThrashCap(time.Now()); !ok || cap != 300 {
		t.Fatalf("the exit must leave the lean start cap (300 of 500), got %d ok=%v", cap, ok)
	}
}

// TestSelfExitLivenessExitsOnStallInAContainer: a container's start script
// treats exit 75 as a planned restart too, and it has no watchdog either, so the
// same recovery must apply there.
func TestSelfExitLivenessExitsOnStallInAContainer(t *testing.T) {
	exited := selfExitLivenessRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go runSelfExitLivenessWatchdog(ctx, livenessGate)

	waitSelfExit(t, exited, "a stalled provider in a container")
}

// TestSelfExitLivenessRefusesWhenTheStateDirIsNotDurable: a container's state
// directory can be a volume that does not survive the restart, and a self-exit
// without its throttle record could loop, so the exit must be refused there.
func TestSelfExitLivenessRefusesWhenTheStateDirIsNotDurable(t *testing.T) {
	exited := selfExitLivenessRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	thrashCheckCapDirWritableFn = func() error { return errors.New("state dir read-only") }

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// TestSelfExitLivenessRespectsTheSelfHealSwitch: with self-heal off the provider
// keeps running and leaves no restart entry behind.
func TestSelfExitLivenessRespectsTheSelfHealSwitch(t *testing.T) {
	exited := selfExitLivenessRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "")

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// TestSelfExitLivenessRefusesWhenTheRestartCannotBeRecorded: the exit is only
// safe with its throttle record on disk, so a write that cannot land (a full
// disk, or a directory that went read-only after the durability probe) must
// refuse the exit rather than restart unthrottled.
func TestSelfExitLivenessRefusesWhenTheRestartCannotBeRecorded(t *testing.T) {
	exited := selfExitLivenessRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	thrashCheckCapDirWritableFn = func() error { return nil } // the probe is satisfied

	// a directory where the cap file belongs: reading sees no entries, writing fails
	dir := filepath.Join(os.Getenv("HOME"), ".urnetwork")
	if err := os.MkdirAll(filepath.Join(dir, "thrash_cap.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// TestLivenessAppliesPinsTheSupervisorsThatGetTheStallDefense: with a systemd
// watchdog there is a ping to withhold; under OpenRC or in a container the
// action is a self-exit; with none of those there is no action to take, so the
// loop must not run.
func TestLivenessAppliesPinsTheSupervisorsThatGetTheStallDefense(t *testing.T) {
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
	thrashEnvFn = func(k string) string {
		if k == "URNETWORK_CONTAINER" {
			return "1"
		}
		return ""
	}
	if !livenessApplies() {
		t.Fatal("in a container the stall defense runs with a self-exit as its action")
	}
	t.Setenv("WATCHDOG_USEC", "1200000000")
	if !livenessApplies() {
		t.Fatal("with WatchdogSec= set the systemd feed runs")
	}
}

// TestRunSdWatchdogDispatchesToSelfExit: with no systemd watchdog the real entry
// point must route to the self-exit path rather than returning early, or the
// stall defense would be silently inert on the hosts it exists for.
func TestRunSdWatchdogDispatchesToSelfExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"openrc", map[string]string{"URNETWORK_INIT": "openrc"}},
		{"container", map[string]string{"URNETWORK_CONTAINER": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exited := selfExitLivenessRig(t, tc.env)
			t.Setenv("URNETWORK_SELF_HEAL", "1")
			t.Setenv("WATCHDOG_USEC", "") // no systemd watchdog on this host

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			go runSdWatchdog(ctx, livenessGate)

			waitSelfExit(t, exited, "runSdWatchdog must dispatch to the self-exit stall path")
		})
	}
}

// TestSelfExitStateDurableOnlyChecksContainers: a container's state directory
// must be writable before a self-exit (its throttle record has to survive);
// systemd and OpenRC keep the file on the box itself, so no probe runs there.
func TestSelfExitStateDurableOnlyChecksContainers(t *testing.T) {
	withTempHome(t)
	prevEnv, prevFiles, prevWritable := thrashEnvFn, thrashFileExistsFn, thrashCheckCapDirWritableFn
	t.Cleanup(func() {
		thrashEnvFn, thrashFileExistsFn, thrashCheckCapDirWritableFn = prevEnv, prevFiles, prevWritable
	})
	probed := 0
	thrashCheckCapDirWritableFn = func() error { probed++; return errors.New("read-only") }

	thrashEnvFn = func(k string) string {
		if k == "URNETWORK_INIT" {
			return "openrc"
		}
		return ""
	}
	thrashFileExistsFn = func(string) bool { return false }
	if err := selfExitStateDurable(); err != nil {
		t.Fatalf("OpenRC keeps the state file on the box: no probe expected, got %v", err)
	}
	if probed != 0 {
		t.Fatalf("no probe must run outside a container, ran %d", probed)
	}

	thrashEnvFn = func(k string) string {
		if k == "URNETWORK_CONTAINER" {
			return "1"
		}
		return ""
	}
	if err := selfExitStateDurable(); err == nil {
		t.Fatal("a container with an unwritable state directory must report not durable")
	}
	if probed != 1 {
		t.Fatalf("the container probe must run exactly once, ran %d", probed)
	}
}
