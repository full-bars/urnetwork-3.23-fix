package main

import (
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

const mib = 1 << 20

// restoreGCProcessState puts back what applyGCLevel changes process-wide, so a
// test that tightens the governor does not leave GOGC low for the rest of the run.
func restoreGCProcessState(t *testing.T) {
	t.Helper()
	// gcGovernor returns early when GOGC is set or the kill switch is on, so
	// every governor test must run with adaptive GC enabled regardless of the
	// environment it inherits.
	t.Setenv("GOGC", "")
	t.Setenv(adaptiveGCDisableEnv, "")
	orig, _ := readGOGCPercent()
	t.Cleanup(func() {
		if orig > 0 {
			debug.SetGCPercent(orig)
		}
		gcTightening.Store(false)
	})
}

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
	score, comps := computePressure(pressureSample{HeapFrac: frac, RunningProxies: 100})
	if score >= 1.0 {
		t.Fatalf("score = %v for live frac %.2f, want below the emergency pin", score, frac)
	}
	if comps["heap"] != 0 {
		t.Fatalf("heap component = %v for live frac %.2f, want none below the ramp", comps["heap"], frac)
	}
}

// Same heapFrac in, same level out, whichever tick feeds it: the 10s subtick
// and the 30s sweep must not disagree about one reading.
func TestSubtickAndSweepAgreeOnOneReading(t *testing.T) {
	restoreGCProcessState(t)
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
	restoreGCProcessState(t)
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
	restoreGCProcessState(t)
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	gcSubtickStep(0.88, 0.9, st)
	if st.level < 2 {
		t.Fatalf("level = %d, want memory pressure above 0.85 to win over the CPU veto", st.level)
	}
}

// Flapping into the critical level must not force a full GC every time.
func TestFreeOSMemoryIsRateLimited(t *testing.T) {
	restoreGCProcessState(t)
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

// The sweep's sample must take its heap fraction from the same source the
// subtick uses. A stub on the shared seam proves the sample reads it.
func TestSampleHeapFracComesFromTheSharedNumerator(t *testing.T) {
	orig := heapFracFn
	defer func() { heapFracFn = orig }()
	heapFracFn = func() float64 { return 0.7777 }
	if got := collectPressureSample().HeapFrac; got != 0.7777 {
		t.Fatalf("sample HeapFrac = %v, want the shared numerator's 0.7777", got)
	}
}

// The FreeOSMemory cooldown must read the single nowFn seam: advancing a fake
// clock past gcFreeOSMemoryMinInterval lets the next critical entry fire,
// with no real waiting. gcNow is an alias that must delegate at call time,
// not capture the time.Now it was initialized with.
func TestNowFnSeamDrivesGCGovernor(t *testing.T) {
	restoreGCProcessState(t)

	oldFree := gcFreeOSMemory
	var calls atomic.Int32
	gcFreeOSMemory = func() { calls.Add(1) }
	t.Cleanup(func() { gcFreeOSMemory = oldFree })

	var fakeNano atomic.Int64
	fakeNano.Store(time.Now().UnixNano())
	oldNow := nowFn
	nowFn = func() time.Time { return time.Unix(0, fakeNano.Load()) }
	t.Cleanup(func() { nowFn = oldNow })

	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100, level: 3}
	applyGCLevel(st)
	if calls.Load() != 1 {
		t.Fatalf("first critical entry: calls = %d, want 1", calls.Load())
	}

	// Re-entering critical inside the interval must not fire again.
	fakeNano.Add(int64(time.Minute))
	st.level = 2
	applyGCLevel(st)
	st.level = 3
	applyGCLevel(st)
	if calls.Load() != 1 {
		t.Fatalf("re-entry within gcFreeOSMemoryMinInterval: calls = %d, want 1", calls.Load())
	}

	// Past the interval on the seam clock, the next critical entry fires.
	fakeNano.Add(int64(gcFreeOSMemoryMinInterval))
	st.level = 2
	applyGCLevel(st)
	st.level = 3
	applyGCLevel(st)
	if calls.Load() != 2 {
		t.Fatalf("re-entry past gcFreeOSMemoryMinInterval: calls = %d, want 2 (the nowFn seam must drive the rate limit)", calls.Load())
	}
}

// gcNow must be an alias of the shared nowFn seam, delegating at call time:
// overriding nowFn reaches the alias, and stubbing the alias directly still
// works without disturbing nowFn. Mirror of TestThrashNowFnFollowsNowFn.
func TestGCNowFollowsNowFn(t *testing.T) {
	oldNow, oldGC := nowFn, gcNow
	t.Cleanup(func() { nowFn, gcNow = oldNow, oldGC })

	viaNow := time.Unix(1_700_000_000, 0)
	nowFn = func() time.Time { return viaNow }
	if got := gcNow(); !got.Equal(viaNow) {
		t.Fatalf("gcNow() = %v after overriding nowFn, want %v: the alias must delegate at call time", got, viaNow)
	}

	direct := time.Unix(1_800_000_000, 0)
	gcNow = func() time.Time { return direct }
	if got := gcNow(); !got.Equal(direct) {
		t.Fatalf("gcNow() = %v after a direct stub, want %v", got, direct)
	}
	if got := nowFn(); !got.Equal(viaNow) {
		t.Fatalf("nowFn() = %v after stubbing the alias, want %v undisturbed", got, viaNow)
	}
}
