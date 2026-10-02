package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetLoopStatus forgets a loop's recorded status so a test that asserts exact
// restart counts also passes under -count=N, where the registry persists.
func resetLoopStatus(name string) {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	delete(loopStatuses, name)
}

func TestNextLoopBackoff(t *testing.T) {
	cases := []struct {
		name         string
		prev, ranFor time.Duration
		want         time.Duration
	}{
		{"first failure", 0, time.Second, loopBackoffBase},
		{"doubles", 2 * time.Second, time.Second, 4 * time.Second},
		{"caps", loopBackoffMax, time.Second, loopBackoffMax},
		{"healthy run resets", loopBackoffMax, loopHealthyRun, loopBackoffBase},
	}
	for _, tc := range cases {
		if got := nextLoopBackoff(tc.prev, tc.ranFor); got != tc.want {
			t.Errorf("%s: nextLoopBackoff(%v, %v) = %v, want %v", tc.name, tc.prev, tc.ranFor, got, tc.want)
		}
	}
}

// A panicking loop must be restarted, with growing backoff, and onFail must
// run after every failure so the loop's actuators fail neutral.
func TestSuperviseLoopRestartsAfterPanic(t *testing.T) {
	resetLoopStatus("test-panic")
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	var sleeps []time.Duration
	loopSleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return ctx.Err() == nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var runs, fails atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseLoop(ctx, "test-panic", func() {
			if runs.Add(1) >= 4 {
				cancel() // stop after the 4th run
				return
			}
			panic("boom")
		}, func() { fails.Add(1) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}

	if got := runs.Load(); got != 4 {
		t.Fatalf("runs = %d, want 4 (3 panics then a clean stop)", got)
	}
	if got := fails.Load(); got != 3 {
		t.Fatalf("onFail calls = %d, want 3", got)
	}
	want := []time.Duration{loopBackoffBase, 2 * loopBackoffBase, 4 * loopBackoffBase}
	if len(sleeps) != 3 || sleeps[0] != want[0] || sleeps[1] != want[1] || sleeps[2] != want[2] {
		t.Fatalf("backoff sleeps = %v, want %v", sleeps, want)
	}
	if st := loopStatusOf("test-panic"); st.Restarts != 3 || st.Up {
		t.Fatalf("status = %+v, want 3 restarts and not up after stop", st)
	}
}

// A loop that returns while the context is live is a failure too, not a clean
// exit: without this a silently ended loop is never noticed.
func TestSuperviseLoopRestartsAfterUnexpectedReturn(t *testing.T) {
	resetLoopStatus("test-return")
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	loopSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }

	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	superviseLoop(ctx, "test-return", func() {
		if runs.Add(1) >= 2 {
			cancel()
		}
	}, nil)
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
	if st := loopStatusOf("test-return"); st.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", st.Restarts)
	}
}

func TestSupervisedLoopMetricsLines(t *testing.T) {
	loopStatusOf("test-metrics") // register
	out := supervisedLoopMetrics()
	for _, want := range []string{
		"# TYPE urnet_loop_restarts_total counter",
		"# TYPE urnet_loop_up gauge",
		`urnet_loop_restarts_total{loop="test-metrics"} 0`,
		`urnet_loop_up{loop="test-metrics"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
}

// A dead pressure monitor must not leave the last (possibly emergency) score,
// tightened GOGC or shrunk memory budget driving the pool controller.
func TestResetPressureActuatorsFailsNeutral(t *testing.T) {
	setPressure(0.95)
	gcTightening.Store(true)
	var setTo []int
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 25, level: 2}
	resetPressureActuators(st, func(n int) int { setTo = append(setTo, n); return 25 })

	if currentPressure() != 0 {
		t.Fatalf("pressure = %v, want 0", currentPressure())
	}
	if gcTightening.Load() {
		t.Fatal("gcTightening still set")
	}
	if len(setTo) != 1 || setTo[0] != 100 || st.currentGOGC != 100 || st.level != 0 {
		t.Fatalf("GOGC reset: set=%v currentGOGC=%d level=%d, want baseline 100 and level 0", setTo, st.currentGOGC, st.level)
	}
}

// When the governor never tightened, the reset must not touch GOGC at all.
func TestResetPressureActuatorsLeavesUntouchedGOGCAlone(t *testing.T) {
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	called := false
	resetPressureActuators(st, func(n int) int { called = true; return 100 })
	if called {
		t.Fatal("SetGCPercent called although GOGC was already at baseline")
	}
}
