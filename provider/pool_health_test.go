package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestNewPoolHealthWindowTickSizing pins the interval-to-ticks conversion: the
// thresholds are durations, so a provider running a non-default
// URNETWORK_HEALTH_INTERVAL must still reason over one hour and over 15/30
// minutes of rise, not over a fixed tick count.
func TestNewPoolHealthWindowTickSizing(t *testing.T) {
	cases := []struct {
		interval                    time.Duration
		window, watchRise, leakRise int
	}{
		{5 * time.Minute, 12, 3, 6},
		{10 * time.Minute, 6, 2, 3},
		{time.Minute, 60, 15, 30},
		{45 * time.Minute, 2, 1, 1},  // ceil: window min 2, rise thresholds min 1
		{0, 12, 3, 6},                // zero interval falls back to 5m
		{-1 * time.Minute, 12, 3, 6}, // negative likewise
	}
	for _, c := range cases {
		w := newPoolHealthWindow(c.interval)
		if w.window != c.window || w.watchRise != c.watchRise || w.leakRise != c.leakRise {
			t.Errorf("interval %s: got window=%d watch=%d leak=%d, want %d/%d/%d",
				c.interval, w.window, w.watchRise, w.leakRise, c.window, c.watchRise, c.leakRise)
		}
	}
}

// TestPoolHealthWarmingUntilWindowFull asserts the window refuses to judge
// before it holds a full hour. A floor computed from three samples is not a
// floor, and guessing a verdict from one is how a healthy provider gets
// reported as leaking.
func TestPoolHealthWarmingUntilWindowFull(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	for tick := 1; tick < w.window; tick++ {
		r := w.observe(uint64(100+tick), 2048, interval)
		if r.Verdict != poolVerdictWarming {
			t.Fatalf("tick %d: got %q, want warming", tick, r.Verdict)
		}
		if r.Full {
			t.Fatalf("tick %d: window reported full before %d samples", tick, w.window)
		}
	}

	r := w.observe(200, 2048, interval)
	if !r.Full {
		t.Fatalf("window not full after %d samples", w.window)
	}
	if r.Verdict == poolVerdictWarming {
		t.Fatal("still warming after a full window of samples")
	}
}

// TestPoolHealthOKWhenFloorFlat feeds a realistic in-flight pattern: in-use
// swings with load but the pool drains to the same baseline every cycle. That
// is the healthy shape and it must never produce a warning.
func TestPoolHealthOKWhenFloorFlat(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	// Two hours of oscillation around a constant floor of 100.
	pattern := []uint64{100, 450, 220, 100, 800, 310, 100, 640, 180, 100, 520, 260}
	var last poolHealthReading
	for cycle := 0; cycle < 2; cycle++ {
		for _, v := range pattern {
			last = w.observe(v, 4096, interval)
		}
	}

	if last.Verdict != poolVerdictOK {
		t.Fatalf("got %q, want ok (floor=%d)", last.Verdict, last.Floor)
	}
	if last.Floor != 100 {
		t.Fatalf("floor=%d, want 100 (the value the pool drains to)", last.Floor)
	}
}

// TestPoolHealthWatchThenLeak walks a leak that starts at boot and pins the
// escalation tick by tick.
//
// It also pins the detection latency, which is a property of the design rather
// than an accident. The floor is the minimum over a trailing hour, so a leak
// that begins at tick 1 does not move the floor until the pre-leak samples age
// out of the window at tick 12. The rise threshold then needs 6 more ticks, so
// a leak is called at tick 18, which is 90 minutes in. That is the cost of a
// metric that never fires on a traffic pattern.
func TestPoolHealthWatchThenLeak(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	got := map[int]string{}
	for tick := 1; tick <= 20; tick++ {
		r := w.observe(uint64(100+tick*40), 2048, interval)
		got[tick] = r.Verdict
	}

	want := map[int]string{
		1: poolVerdictWarming, 11: poolVerdictWarming, // window not yet full
		12: poolVerdictOK, 14: poolVerdictOK, // full, floor has not moved yet
		15: poolVerdictWatch, 17: poolVerdictWatch, // floor rising 3-5 ticks
		18: poolVerdictLeak, 20: poolVerdictLeak, // 30 minutes of rise
	}
	for tick, expected := range want {
		if got[tick] != expected {
			t.Errorf("tick %d: got %q, want %q", tick, got[tick], expected)
		}
	}
}

// TestPoolHealthRiseStreakResets asserts a single drop clears the streak. A
// provider whose load steps up and settles must not accumulate toward a leak
// verdict across unrelated step-ups.
func TestPoolHealthRiseStreakResets(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	// Climb until the floor has been rising long enough to warrant a watch.
	var last poolHealthReading
	for tick := 1; tick <= w.window+w.watchRise; tick++ {
		last = w.observe(uint64(100+tick*40), 1024, interval)
	}
	if last.Verdict != poolVerdictWatch {
		t.Fatalf("setup verdict %q, want watch", last.Verdict)
	}

	// The pool gives buffers back: in-use drops below the window minimum, so
	// the floor drops with it and the streak must clear.
	r := w.observe(50, 1024, interval)
	if r.RisingFor != 0 {
		t.Fatalf("RisingFor=%s after a drop, want 0", r.RisingFor)
	}
	if r.Floor != 50 {
		t.Fatalf("floor=%d after a drop to 50, want 50", r.Floor)
	}

	// Climbing again starts from zero, so nothing here reaches a leak verdict.
	for tick := 1; tick <= w.leakRise; tick++ {
		last = w.observe(uint64(50+tick*10), 1024, interval)
		if last.Verdict == poolVerdictLeak {
			t.Fatalf("tick %d after the drop reported a leak: streak carried across the drop", tick)
		}
	}
}

// TestPoolHealthCatchesProportionalLeak is the regression test for the metric
// choice itself. A leak proportional to throughput keeps returned/taken pinned
// above 99.9% forever, because the denominator grows exactly as fast as the
// leak. Any verdict keyed on that percentage stays silent while the provider
// bleeds buffers. The floor must catch it anyway.
func TestPoolHealthCatchesProportionalLeak(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	const inFlight = 400 // buffers legitimately in use at any moment
	var taken, returned, leaked uint64
	var last poolHealthReading
	var lastPct float64

	// Three hours at 100k buffers per tick, losing 1 in 10,000.
	for tick := 0; tick < 36; tick++ {
		taken += 100_000
		leaked += 10
		returned = taken - leaked
		last = w.observe(leaked+inFlight, 4096, interval)
		lastPct = 100 * float64(returned) / float64(taken)
	}

	if lastPct < 99.9 {
		t.Fatalf("return percentage %.4f%%, expected the leak to stay hidden above 99.9%%", lastPct)
	}
	if last.Verdict != poolVerdictLeak {
		t.Fatalf("got %q at %.4f%% returned, want leak: the floor must catch what the percentage hides",
			last.Verdict, lastPct)
	}
	if last.Floor <= inFlight {
		t.Fatalf("floor=%d, want above the %d in-flight baseline", last.Floor, inFlight)
	}
}

// TestPoolHealthCreatedGrowthWatch covers the second leak shape: in-use stays
// flat but the pool keeps allocating, which means recycled buffers are not
// coming back to be reused. Allocation should plateau once the working set is
// covered.
func TestPoolHealthCreatedGrowthWatch(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	created := uint64(2048)
	var last poolHealthReading
	for i := 0; i < w.window+w.leakRise+1; i++ {
		created += 64 // never plateaus
		last = w.observe(100, created, interval)
	}

	if last.Verdict != poolVerdictWatch {
		t.Fatalf("got %q, want watch on continuous allocation growth", last.Verdict)
	}
	if !last.CreatedRising {
		t.Fatal("CreatedRising false while created grew every tick")
	}
}

// TestPoolHealthLineWording asserts each verdict renders text that explains
// itself. The line is read by operators who have never opened message_pool.go,
// so it must define "stuck" in place and name the consequence.
func TestPoolHealthLineWording(t *testing.T) {
	cases := []struct {
		name     string
		reading  poolHealthReading
		inUse    uint64
		contains []string
		absent   []string
	}{
		{
			name:     "ok stays short",
			reading:  poolHealthReading{Verdict: poolVerdictOK, Floor: 0, Full: true},
			inUse:    117,
			contains: []string{"ok —", "117 buffers in use", "none stuck", "2048 allocated since start", "99.65% returned"},
			absent:   []string{"bug", "restart"},
		},
		{
			name:     "ok with a steady nonzero floor says so",
			reading:  poolHealthReading{Verdict: poolVerdictOK, Floor: 38, Full: true},
			inUse:    117,
			contains: []string{"38 stuck but holding steady"},
		},
		{
			name:     "warming explains why there is no verdict",
			reading:  poolHealthReading{Verdict: poolVerdictWarming, WarmingRemaining: 55 * time.Minute},
			inUse:    117,
			contains: []string{"warming —", "needs 1h of uptime", "55m to go"},
		},
		{
			name: "watch names the trend and the consequence",
			reading: poolHealthReading{Verdict: poolVerdictWatch, Floor: 96,
				FloorAtWindowStart: 38, RisingFor: 20 * time.Minute, Full: true},
			inUse: 1204,
			contains: []string{"watch —", "taken and never given back", "up from 38 an hour ago",
				"memory use grows until the provider restarts", "1204 in use now"},
		},
		{
			name: "leak says it is a bug and how long",
			reading: poolHealthReading{Verdict: poolVerdictLeak, Floor: 512,
				FloorAtWindowStart: 38, RisingFor: 35 * time.Minute, Full: true},
			inUse: 4291,
			contains: []string{"leak —", "512 buffers taken and never given back",
				"climbing for 35+ minutes", "bug worth reporting", "4291 in use now"},
		},
		{
			name: "watch on allocation growth uses the allocation wording",
			reading: poolHealthReading{Verdict: poolVerdictWatch, Floor: 38,
				FloorAtWindowStart: 38, CreatedRising: true, Full: true},
			inUse:    117,
			contains: []string{"keeps allocating new buffers", "not coming back"},
			absent:   []string{"up from 38 an hour ago"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := poolHealthLine(c.reading, c.inUse, 2048, 33740, 33622)
			for _, want := range c.contains {
				if !strings.Contains(line, want) {
					t.Errorf("line missing %q:\n%s", want, line)
				}
			}
			for _, unwanted := range c.absent {
				if strings.Contains(line, unwanted) {
					t.Errorf("line should not contain %q:\n%s", unwanted, line)
				}
			}
			// No line may leak internal field names at the operator.
			for _, jargon := range []string{"floor_1h", "outstanding=", "reuse=", "buckets="} {
				if strings.Contains(line, jargon) {
					t.Errorf("line contains jargon %q:\n%s", jargon, line)
				}
			}
		})
	}
}

// TestPoolHealthLineZeroTaken guards the division: a tick that fires before
// anything has been taken must not render NaN at an operator.
func TestPoolHealthLineZeroTaken(t *testing.T) {
	line := poolHealthLine(poolHealthReading{Verdict: poolVerdictOK, Full: true}, 0, 0, 0, 0)
	if strings.Contains(line, "NaN") {
		t.Fatalf("NaN in line: %s", line)
	}
	if !strings.Contains(line, "0.00% returned") {
		t.Fatalf("want a zero percentage, got: %s", line)
	}
}

// TestPoolHealthWarmingRemainingNeverZero asserts the countdown stays sensible
// at the boundary, where truncation would otherwise print "0s to go" on the
// tick before the window fills.
func TestPoolHealthWarmingRemainingNeverZero(t *testing.T) {
	for _, remaining := range []time.Duration{0, time.Second, time.Minute} {
		line := poolHealthLine(poolHealthReading{Verdict: poolVerdictWarming, WarmingRemaining: remaining}, 10, 64, 100, 90)
		if strings.Contains(line, "(0s to go)") {
			t.Errorf("WarmingRemaining %s produced a zero countdown: %s", remaining, line)
		}
	}
}

// TestPoolHealthDelayedActivityWarmingRemaining asserts the warming countdown
// is derived from sample count, not uptime.  When pool activity starts after
// the one-hour uptime mark, the countdown still reflects the missing samples
// rather than showing a zero.
func TestPoolHealthDelayedActivityWarmingRemaining(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval)

	// Fire 11 ticks with zero pool activity (totalTaken == 0 in the caller,
	// so observe is not called).  Now simulate the first pool activity tick:
	// only 1 sample in the ring, but uptime is already 55 minutes.
	w.inUse = appendCapped(w.inUse, 42, w.window)
	w.floors = appendCapped(w.floors, 42, w.window)
	w.seeded = true

	remaining := time.Duration(w.window-len(w.inUse)) * interval
	if remaining < time.Minute {
		remaining = time.Minute
	}
	r := poolHealthReading{
		Verdict:          poolVerdictWarming,
		WarmingRemaining: remaining,
	}
	if r.WarmingRemaining != 55*time.Minute {
		t.Fatalf("WarmingRemaining=%s, want 55m (window needs %d more samples)", r.WarmingRemaining, w.window-len(w.inUse))
	}

	// Render and confirm the countdown is 55m, not 5m.
	line := poolHealthLine(r, 42, 64, 100, 90)
	if !strings.Contains(line, "55m to go") {
		t.Errorf("expected 55m to go, got: %s", line)
	}
}

// TestPoolHealthObserveWarmingRemaining asserts observe() computes
// WarmingRemaining from the sample count.
func TestPoolHealthObserveWarmingRemaining(t *testing.T) {
	interval := 5 * time.Minute
	w := newPoolHealthWindow(interval) // window=12

	r := w.observe(100, 2048, interval) // 1 sample
	if r.WarmingRemaining != 55*time.Minute {
		t.Errorf("tick 1: WarmingRemaining=%s, want 55m", r.WarmingRemaining)
	}

	// Fill 11 more (total 12 = window).
	for i := 2; i <= 12; i++ {
		r = w.observe(uint64(100+i), 2048, interval)
	}
	if r.WarmingRemaining != 0 {
		t.Errorf("tick 12: WarmingRemaining=%s, want 0 (window is full)", r.WarmingRemaining)
	}
	if !r.Full {
		t.Error("window should be full after 12 samples")
	}
}

func TestRoundMinutes(t *testing.T) {
	cases := map[time.Duration]string{
		0:                               "1+ minutes",
		30 * time.Second:                "1+ minutes",
		15 * time.Minute:                "15+ minutes",
		35 * time.Minute:                "35+ minutes",
		90*time.Minute + 20*time.Second: "90+ minutes",
	}
	for d, want := range cases {
		if got := roundMinutes(d); got != want {
			t.Errorf("roundMinutes(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestAppendCappedKeepsNewest(t *testing.T) {
	var s []uint64
	for i := 1; i <= 5; i++ {
		s = appendCapped(s, uint64(i), 3)
	}
	if got := fmt.Sprint(s); got != "[3 4 5]" {
		t.Fatalf("got %s, want [3 4 5]", got)
	}
	if len(s) != 3 {
		t.Fatalf("len=%d, want 3", len(s))
	}
}

func TestMinUint64(t *testing.T) {
	if got := minUint64(nil); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}
	if got := minUint64([]uint64{7}); got != 7 {
		t.Errorf("single: got %d, want 7", got)
	}
	if got := minUint64([]uint64{9, 3, 8, 3, 11}); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

// TestHostLabelFromPrecedence covers the label chain the build line uses.
// Precedence is node name, then HOST_HOSTNAME, then the kernel hostname, and
// every candidate is trimmed so a stray newline in an override file does not
// break the line.
func TestHostLabelFromPrecedence(t *testing.T) {
	cases := []struct {
		nodeName, hostEnv, hostname, want string
	}{
		{"node-a", "docker-host", "box", "node-a"},
		{"", "RC-TEST", "box", "RC-TEST"},
		{"", "", "box", "box"},
		{"  ", "\n", "  box  ", "box"},
		{"  node-a\n", "RC-TEST", "box", "node-a"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := hostLabelFrom(c.nodeName, c.hostEnv, c.hostname); got != c.want {
			t.Errorf("hostLabelFrom(%q, %q, %q) = %q, want %q",
				c.nodeName, c.hostEnv, c.hostname, got, c.want)
		}
	}
}
