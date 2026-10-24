//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// hangExitPathSeams makes every step of the pre-exit path that could touch a
// hung state directory block until the returned restore func runs: the marker
// write, the status-file clear, and the stderr sink (a wedged journal pipe, or
// stderr redirected onto the same hung volume). The bounded waits are shrunk so
// the test does not sit through the production timeouts.
func hangExitPathSeams(t *testing.T) (markerCalls *atomic.Int32, restore func()) {
	t.Helper()
	release := make(chan struct{})
	prevMarker, prevStderr, prevClear := thrashExitMarkerWriteFn, thrashStderrWriteFn, thrashStatusClearFn
	prevPersist, prevNote := thrashPersistTimeout, thrashExitNoteTimeout

	markerCalls = &atomic.Int32{}
	thrashExitMarkerWriteFn = func() error {
		markerCalls.Add(1)
		<-release
		return nil
	}
	thrashStatusClearFn = func() { <-release }
	thrashStderrWriteFn = func(p []byte) (int, error) {
		<-release
		return len(p), nil
	}
	thrashPersistTimeout = 25 * time.Millisecond
	thrashExitNoteTimeout = 25 * time.Millisecond

	return markerCalls, func() {
		close(release)
		thrashExitMarkerWriteFn, thrashStderrWriteFn, thrashStatusClearFn = prevMarker, prevStderr, prevClear
		thrashPersistTimeout, thrashExitNoteTimeout = prevPersist, prevNote
	}
}

// The pre-exit diagnostics are stderr-only: they must not append to (and fsync)
// events.log in the state directory whose hang fires the escalation, because a
// bounded marker write followed by an events.log write on its failure path would
// just move the block one line later and still keep the node from restarting.
// Checked on both branches: the reason line, and the marker-failure line.
func TestThrashExitDiagnosticsBypassTheStateDirectory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		markerErr error
		want      string
	}{
		{"healthy marker", nil, "restarting the provider now"},
		{"marker write fails", errors.New("timed out writing the exit marker"), "could not write the exit marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTempHome(t)
			prevStderr, prevExit, prevMarker := thrashStderrWriteFn, thrashExitFn, thrashExitMarkerWriteFn
			t.Cleanup(func() {
				thrashStderrWriteFn, thrashExitFn, thrashExitMarkerWriteFn = prevStderr, prevExit, prevMarker
			})

			var lines []string
			thrashStderrWriteFn = func(p []byte) (int, error) {
				lines = append(lines, string(p))
				return len(p), nil
			}
			exitCode := -1
			thrashExitFn = func(code int) { exitCode = code }
			if tc.markerErr != nil {
				thrashExitMarkerWriteFn = func() error { return tc.markerErr }
			}

			thrashExitOnRestart("[thrash]", "🚨 [memory] restarting the provider now")

			if exitCode != thrashExitCode {
				t.Fatalf("exit code = %d, want thrashExitCode (%d)", exitCode, thrashExitCode)
			}
			found := false
			for _, l := range lines {
				if strings.Contains(l, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("the %q line must reach stderr, got %q", tc.want, lines)
			}
			if _, err := os.Stat(filepath.Join(home, ".urnetwork", "events.log")); err == nil {
				t.Fatal("the pre-exit path wrote events.log in the state directory")
			}
		})
	}
}

// The hole this file exists for: a marker write that times out used to be
// followed by a critLog/importantLogf line that fsyncs events.log in the same
// hung state directory, so the exit — and therefore the supervised restart —
// never happened. events.log is a FIFO here, so any critLog on this path blocks
// forever in open(2), exactly like the state directory stuck in swap I/O; the
// marker, the status clear and stderr are hung too. The exit must still be taken
// promptly.
func TestThrashExitOnRestartSurvivesAHungStateDirectory(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A FIFO with no reader: os.OpenFile(O_WRONLY) blocks, which is what a
	// critLog against a hung state directory does.
	if err := syscall.Mkfifo(filepath.Join(dir, "events.log"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	markerCalls, restore := hangExitPathSeams(t)
	defer restore()
	prevExit := thrashExitFn
	t.Cleanup(func() { thrashExitFn = prevExit })

	exited := make(chan int, 1)
	thrashExitFn = func(code int) { exited <- code }

	start := time.Now()
	go thrashExitOnRestart("[thrash]", "🚨 [memory] restarting the provider now")

	select {
	case code := <-exited:
		if code != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", code, thrashExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a hung log path stopped the exit: thrashExitFn was never called")
	}
	took := time.Since(start)

	if markerCalls.Load() == 0 {
		t.Fatal("the marker write was never attempted, so the timeout path was not exercised")
	}
	if took > time.Second {
		t.Fatalf("the exit path was not bounded: took %v", took)
	}
}

// The producer, not just the sequence: with the whole exit path hung, the
// watchdog loop must still reach thrashExitFn with 75. This is what a state
// directory stuck in swap I/O looks like on the node the escalation exists for.
func TestThrashWatchdogExitsWhenTheExitPathHangs(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("INVOCATION_ID", "test-supervisor")
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
	restoreDurations := shrinkThrashDurations()
	t.Cleanup(restoreDurations)

	calls := 0
	var psiFull, swapIn uint64
	prevRead := thrashReadFn
	thrashReadFn = func() thrashRead {
		calls++
		if calls == 1 {
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

	markerCalls, restoreSeams := hangExitPathSeams(t)
	t.Cleanup(restoreSeams)
	prevExit := thrashExitFn
	t.Cleanup(func() { thrashExitFn = prevExit })
	exited := make(chan int, 1)
	thrashExitFn = func(code int) { exited <- code }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runThrashWatchdog(ctx, true)
		close(done)
	}()

	select {
	case code := <-exited:
		if code != thrashExitCode {
			t.Fatalf("exit code = %d, want thrashExitCode (%d)", code, thrashExitCode)
		}
	case <-time.After(4 * time.Second):
		cancel()
		<-done
		t.Fatal("the watchdog never reached the exit with its state directory hung")
	}
	if markerCalls.Load() == 0 {
		t.Fatal("the marker write was never attempted")
	}
}
