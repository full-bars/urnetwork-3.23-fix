package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSdWatchdogInterval(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if _, ok := sdWatchdogInterval(env(nil)); ok {
		t.Fatal("no WATCHDOG_USEC: the watchdog must stay off")
	}
	if _, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "0"})); ok {
		t.Fatal("WATCHDOG_USEC=0 means disabled")
	}
	if _, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "garbage"})); ok {
		t.Fatal("an unparseable WATCHDOG_USEC must stay off")
	}
	// systemd's own advice: ping at half the timeout; a third leaves margin for
	// one late tick on a loaded box
	got, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "1200000000"}))
	if !ok || got != 400*time.Second {
		t.Fatalf("1200s timeout must ping every 400s, got %v ok=%v", got, ok)
	}
	// a tiny timeout still gets a sane floor
	if got, ok := sdWatchdogInterval(env(map[string]string{"WATCHDOG_USEC": "900000"})); !ok || got < time.Second {
		t.Fatalf("the interval must have a one second floor, got %v ok=%v", got, ok)
	}
}

func TestLivenessProgressIsFreshOnlyWhileTheLoopTicks(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	l := newLivenessProgress(func() time.Time { return time.Unix(0, clock.Load()) })

	if !l.fresh(10 * time.Minute) {
		t.Fatal("a process that just started has made progress by definition")
	}
	clock.Add(int64(9 * time.Minute))
	if !l.fresh(10 * time.Minute) {
		t.Fatal("9 minutes without a tick is inside the 10 minute budget")
	}
	clock.Add(int64(2 * time.Minute))
	if l.fresh(10 * time.Minute) {
		t.Fatal("11 minutes without a tick must read as stalled")
	}
	l.note()
	if !l.fresh(10 * time.Minute) {
		t.Fatal("a tick must restore freshness")
	}
}

// The ping is the whole point: it must keep flowing while progress is made and
// stop while it is not, so systemd restarts a process that cannot make progress.
func TestRunSdWatchdogWithholdsThePingWhileStalled(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	l := newLivenessProgress(now)

	var mu sync.Mutex
	var pings, logs, stalls, undos int
	ping := func() error { mu.Lock(); pings++; mu.Unlock(); return nil }
	logf := func(string, ...any) { mu.Lock(); logs++; mu.Unlock() }
	tick := make(chan time.Time)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSdWatchdogLoop(ctx, tick, l, 10*time.Minute, ping, logf, func() bool { return true }, func() func() { mu.Lock(); stalls++; mu.Unlock(); return func() { mu.Lock(); undos++; mu.Unlock() } })
	}()
	step := func() {
		t.Helper()
		select {
		case tick <- now():
		case <-time.After(2 * time.Second):
			t.Fatal("the watchdog loop did not take the tick")
		}
		// the loop handles a tick before it takes the next one
		select {
		case tick <- now():
		case <-time.After(2 * time.Second):
			t.Fatal("the watchdog loop did not finish the tick")
		}
	}
	count := func() (int, int) { mu.Lock(); defer mu.Unlock(); return pings, logs }

	step() // healthy
	if p, _ := count(); p < 1 {
		t.Fatalf("a healthy process must be pinged, pings=%d", p)
	}
	healthyPings, _ := count()

	clock.Add(int64(11 * time.Minute)) // no tick for 11 minutes
	step()
	step()
	p, lg := count()
	if p != healthyPings {
		t.Fatalf("a stalled process must NOT be pinged: pings went %d -> %d", healthyPings, p)
	}
	if lg != 1 {
		t.Fatalf("the stall must be logged exactly once per episode, logged %d times", lg)
	}
	mu.Lock()
	if stalls != 1 {
		mu.Unlock()
		t.Fatalf("onStall must run exactly once per episode, ran %d times", stalls)
	}
	mu.Unlock()

	l.note() // the loop ticks again
	step()
	if p, _ := count(); p <= healthyPings {
		t.Fatal("pinging must resume once progress resumes")
	}
	mu.Lock()
	if undos != 1 {
		mu.Unlock()
		t.Fatalf("the lean cap written at the stall must be undone when progress resumes before systemd acts, undone %d times", undos)
	}
	mu.Unlock()
	cancel()
	<-done
}

// A stall leaves a lean start cap behind, like a thrash escape, so the restart
// systemd performs does not walk back into the same spiral.
func TestRecordLivenessStallLeavesALeanStartCap(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	recordLivenessStall()

	cap, ok := activeThrashCap(time.Now())
	if !ok || cap != 300 {
		t.Fatalf("expected an active cap of 300 (60%% of 500), got %d ok=%v", cap, ok)
	}
	if n := len(thrashRestartsWithin(readThrashCapState(), time.Now())); n != 1 {
		t.Fatalf("the stall must count in the anti-loop ring, ring has %d entries", n)
	}
}

// self-heal off means off for actions: the watchdog is still fed, nothing is
// recorded, and the line says what would have happened.
func TestRunSdWatchdogWithSelfHealOffKeepsFeedingAndRecordsNothing(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	l := newLivenessProgress(now)

	var mu sync.Mutex
	var pings, stalls int
	var lines []string
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSdWatchdogLoop(ctx, tick, l, 10*time.Minute,
			func() error { mu.Lock(); pings++; mu.Unlock(); return nil },
			func(f string, a ...any) { mu.Lock(); lines = append(lines, f); mu.Unlock() },
			func() bool { return false },
			func() func() { mu.Lock(); stalls++; mu.Unlock(); return nil })
	}()
	clock.Add(int64(11 * time.Minute))
	for i := 0; i < 4; i++ {
		tick <- now()
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if pings < 3 {
		t.Fatalf("with self-heal off the watchdog must keep being fed, pings=%d", pings)
	}
	if stalls != 0 {
		t.Fatalf("with self-heal off nothing may be recorded, onStall ran %d times", stalls)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "self-heal is off") {
		t.Fatalf("expected exactly one line saying self-heal is off, got %v", lines)
	}
}

func TestRecordLivenessStallCanBeUndone(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	before := readThrashCapState()
	undo := recordLivenessStall()
	if _, ok := activeThrashCap(time.Now()); !ok {
		t.Fatal("the stall must leave a cap in place until undone")
	}
	undo()
	after := readThrashCapState()
	if after.Cap != before.Cap || len(after.Restarts) != len(before.Restarts) {
		t.Fatalf("undo must restore the previous cap state, before=%+v after=%+v", before, after)
	}
	if _, ok := activeThrashCap(time.Now()); ok {
		t.Fatal("no cap may remain after the stall resolved itself")
	}
}
