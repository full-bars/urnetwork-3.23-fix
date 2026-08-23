package main

import (
	"context"
	"testing"
	"time"
)

// Coverage-gap test (Sonnet round-3 review): runPaidProxyGrader's ticker loop
// was 0% covered — every existing paid-grader test drives runPaidProxyGradeOnce
// directly, never the wrapping ticker/select loop that production actually
// runs. proxyReaperInterval is 5 minutes, far too long to wait out in a unit
// test, so this drives the loop with an ALREADY-CANCELLED context: the
// `select { case <-ctx.Done(): return; case <-ticker.C: }` must take the
// ctx.Done() branch and return promptly, never blocking on the ticker.

// TestRunPaidProxyGrader_ExitsOnCancelledContext pins that the ticker loop
// exits immediately when its context is already done, without waiting for
// proxyReaperInterval (5m) to elapse and without running a probe pass.
func TestRunPaidProxyGrader_ExitsOnCancelledContext(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before runPaidProxyGrader is ever entered

	done := make(chan struct{})
	go func() {
		runPaidProxyGrader(ctx, "1.2.3.4", 443)
		close(done)
	}()

	select {
	case <-done:
		// Correct: the loop observed ctx.Done() on its first select and
		// returned without waiting for a 5-minute ticker tick.
	case <-time.After(3 * time.Second):
		t.Fatal("runPaidProxyGrader did not exit promptly on an already-cancelled context (blocked on the 5m ticker instead of ctx.Done())")
	}
}

// TestRunPaidProxyGrader_CancelMidRunStopsLoop pins the SECOND way the loop
// must exit: a context cancelled WHILE the loop is alive (not just already
// dead at entry) must also stop it, exercising the select choosing ctx.Done()
// over a live (not-yet-fired) ticker channel, and confirming ticker.Stop() via
// defer does not leak a goroutine that blocks process shutdown.
func TestRunPaidProxyGrader_CancelMidRunStopsLoop(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runPaidProxyGrader(ctx, "1.2.3.4", 443)
		close(done)
	}()

	// Give the goroutine a moment to enter its select and start waiting on
	// the ticker/ctx, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runPaidProxyGrader did not exit after its context was cancelled mid-run")
	}
}
