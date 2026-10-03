package main

import (
	"runtime/debug"
	"testing"
)

func armGovernorTest(t *testing.T, startGOGC int) *gcGovernorState {
	t.Helper()
	t.Setenv("GOGC", "")
	t.Setenv(adaptiveGCDisableEnv, "")
	orig := debug.SetGCPercent(startGOGC)
	t.Cleanup(func() { debug.SetGCPercent(orig); gcTightening.Store(false) })
	// The state the monitor builds when it arms: baseline read from the runtime.
	return &gcGovernorState{baselineGOGC: startGOGC, currentGOGC: startGOGC}
}

func releaseGovernor(t *testing.T, state *gcGovernorState) {
	t.Helper()
	for i := 0; i < 4*3 && state.level != 0; i++ {
		gcSelfHealOffTick(0.10, 4000, 0, state)
	}
	if state.level != 0 {
		t.Fatalf("calm ticks must release to level 0, stuck at %d", state.level)
	}
}

// The monitor captures its GOGC baseline when it arms, but the auto tier writes
// its value once per process at the first proxy launch, which can come later on
// a box whose first proxy is delayed. A baseline captured before that write is
// stale: the first tighten-then-release used to put 100 back over the tier's
// value, and nothing restored it (32.7 re-applied the tier on every launch).
func TestGCGovernorAdoptsATierGOGCThatLandsAfterItArmed(t *testing.T) {
	state := armGovernorTest(t, 100)

	debug.SetGCPercent(50) // the tier default lands after the monitor armed
	gcGovernor(0.10, -1, 0, false, state)
	if state.baselineGOGC != 50 {
		t.Fatalf("baseline must adopt the GOGC the governor did not write, got %d", state.baselineGOGC)
	}

	gcGovernor(0.95, -1, 0, false, state) // spike: tighten
	if state.level != 3 {
		t.Fatalf("spike should reach level 3, got %d", state.level)
	}
	releaseGovernor(t, state)
	if got := debug.SetGCPercent(50); got != 50 {
		t.Fatalf("after a tighten and release the GOGC must be the tier value 50, got %d", got)
	}
}

// While the governor is tightening it owns GOGC, so its own lowered value must
// not be mistaken for an external write and adopted as the baseline.
func TestGCGovernorDoesNotAdoptItsOwnTightenedValue(t *testing.T) {
	state := armGovernorTest(t, 100)

	gcGovernor(0.95, -1, 0, false, state)
	if state.level != 3 || state.currentGOGC != 10 {
		t.Fatalf("expected level 3 at GOGC 10, got level %d GOGC %d", state.level, state.currentGOGC)
	}
	gcGovernor(0.95, -1, 0, false, state)
	if state.baselineGOGC != 100 {
		t.Fatalf("a tightened value must never become the baseline, got %d", state.baselineGOGC)
	}
}

// An explicit URNETWORK_BASELINE_GOGC is the operator's word and is never
// replaced by whatever another writer sets afterwards.
func TestGCGovernorKeepsAPinnedBaseline(t *testing.T) {
	state := armGovernorTest(t, 80)
	state.baselinePinned = true

	debug.SetGCPercent(50)
	gcGovernor(0.10, -1, 0, false, state)
	if state.baselineGOGC != 80 {
		t.Fatalf("a pinned baseline must stay, got %d", state.baselineGOGC)
	}
}
