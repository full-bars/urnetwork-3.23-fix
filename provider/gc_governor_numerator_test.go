package main

import (
	"testing"
	"time"
)

const mib = 1 << 20

// The numerator must be what the process is actually holding (live heap plus
// stacks), not the heap goal. The goal moves with GOGC, which is what the
// governor itself writes, so a goal-based reading chases its own tail.
func TestHeapFracFromLiveHeapAndStacks(t *testing.T) {
	got := heapFracFrom(300*mib, 20*mib, 400*mib)
	if !almostEq(got, 0.8) {
		t.Fatalf("heapFracFrom = %v, want 0.8", got)
	}
	if heapFracFrom(0, 0, 400*mib) != 0 {
		t.Fatal("no live heap reading must yield 0, not a spurious fraction")
	}
	if heapFracFrom(300*mib, 20*mib, 0) != 0 {
		t.Fatal("no limit must yield 0")
	}
}

// A box whose runtime memory simply sits at its soft limit (big heap goal,
// small live set) must not read as an emergency.
func TestHealthyBoxAtSoftLimitIsNotAnEmergency(t *testing.T) {
	frac := heapFracFrom(120*mib, 10*mib, 680*mib) // live is small; the goal would be near 680
	score, _ := computePressure(pressureSample{HeapFrac: frac, RunningProxies: 100})
	if score >= 1.0 {
		t.Fatalf("score = %v for live frac %.2f, want below the emergency pin", score, frac)
	}
}

// Same heapFrac in, same level out, whichever tick feeds it: the 10s subtick
// and the 30s sweep must not disagree about one reading.
func TestSubtickAndSweepAgreeOnOneReading(t *testing.T) {
	sub := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	sweep := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	gcSubtickStep(0.75, 0, sub)
	gcGovernor(0.75, -1, 0, true, sweep)
	if sub.level != sweep.level || sub.currentGOGC != sweep.currentGOGC {
		t.Fatalf("subtick level=%d gogc=%d, sweep level=%d gogc=%d", sub.level, sub.currentGOGC, sweep.level, sweep.currentGOGC)
	}
}

// During a CPU-bound episode the sweep refuses to tighten on a moderate heap
// reading. The subtick must honour the same veto, or it re-tightens within
// 10s of every release.
func TestSubtickHonoursCPUVeto(t *testing.T) {
	vetoed := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	gcSubtickStep(0.75, 0.9, vetoed)
	if vetoed.level != 0 {
		t.Fatalf("level = %d under CPU veto, want 0", vetoed.level)
	}
	free := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	gcSubtickStep(0.75, 0, free)
	if free.level != 1 {
		t.Fatalf("level = %d without CPU pressure, want 1", free.level)
	}
}

// Memory above 0.85 of the budget overrides the CPU veto, as in the sweep.
func TestSubtickCPUVetoYieldsToRealMemoryPressure(t *testing.T) {
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	gcSubtickStep(0.88, 0.9, st)
	if st.level < 2 {
		t.Fatalf("level = %d, want memory pressure above 0.85 to win over the CPU veto", st.level)
	}
}

// Flapping into the critical level must not force a full GC every time.
func TestFreeOSMemoryIsRateLimited(t *testing.T) {
	calls := 0
	origFree, origNow := gcFreeOSMemory, gcNow
	defer func() { gcFreeOSMemory, gcNow = origFree, origNow }()
	gcFreeOSMemory = func() { calls++ }
	now := time.Unix(1_700_000_000, 0)
	gcNow = func() time.Time { return now }

	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100, level: 3}
	applyGCLevel(st)
	if calls != 1 {
		t.Fatalf("first critical entry: calls = %d, want 1", calls)
	}
	st.level = 2
	applyGCLevel(st)
	st.level = 3
	now = now.Add(time.Minute)
	applyGCLevel(st)
	if calls != 1 {
		t.Fatalf("re-entry within the interval: calls = %d, want still 1", calls)
	}
	now = now.Add(gcFreeOSMemoryMinInterval)
	st.level = 2
	applyGCLevel(st)
	st.level = 3
	applyGCLevel(st)
	if calls != 2 {
		t.Fatalf("re-entry after the interval: calls = %d, want 2", calls)
	}
}
