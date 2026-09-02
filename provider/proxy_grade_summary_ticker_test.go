package main

import (
	"context"
	"testing"
	"time"
)

// Coverage-gap tests: runProxyGradeSummary's ticker
// loop was 0% covered — every existing summary test drives
// runProxyGradeSummaryOnce directly. Same technique as the paid-grader ticker
// tests: an already-cancelled (or cancelled-mid-run) context must make the
// `select { case <-ctx.Done(): return; case <-ticker.C: ... }` loop exit
// promptly instead of blocking on the (default 5-minute) summary interval.

// TestRunProxyGradeSummary_ExitsOnCancelledContext pins that the loop exits
// immediately on an already-done context, without waiting out the interval.
func TestRunProxyGradeSummary_ExitsOnCancelledContext(t *testing.T) {
	withTempHome(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		runProxyGradeSummary(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runProxyGradeSummary did not exit promptly on an already-cancelled context")
	}
}

// TestRunProxyGradeSummary_CancelMidRunStopsLoop pins cancellation while the
// loop is alive and waiting in its select, not just already-dead at entry.
func TestRunProxyGradeSummary_CancelMidRunStopsLoop(t *testing.T) {
	withTempHome(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runProxyGradeSummary(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runProxyGradeSummary did not exit after its context was cancelled mid-run")
	}
}

// TestRunProxyGradeSummary_PicksUpConfiguredInterval is a companion positive
// check on summaryIntervalFromConfig feeding the ticker construction: writing
// a short custom interval before the loop starts must be what NewTicker is
// built with (proven indirectly: the loop must still be exit-on-cancel
// clean with a non-default interval configured, i.e. reading the config at
// startup does not itself break the loop).
func TestRunProxyGradeSummary_PicksUpConfiguredInterval(t *testing.T) {
	writeGradesOverride(t, `{"interval_sec": 1}`)
	resetProxyGradesConfigCache()
	if got := summaryIntervalFromConfig(); got != time.Second {
		t.Fatalf("summaryIntervalFromConfig() = %v, want 1s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runProxyGradeSummary(ctx)
		close(done)
	}()
	// With a 1s interval the loop should tick at least once before we cancel
	// at 1.5s; either way it must still exit promptly after cancel.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runProxyGradeSummary with a short configured interval did not exit after cancel")
	}
}
