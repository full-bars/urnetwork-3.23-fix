package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// The GC governor is a memory-safety actuator that runs whether or not
// self-heal is on, but only the full 30s sweep may RELEASE a tightened level,
// and that sweep was skipped when self-heal was off. GC therefore ratcheted
// tighter for the rest of the process's life. The self-heal-off tick must run
// the same calm-sample release path, and every level change must be logged.
func TestGCSelfHealOffTickReleasesTightenedLevel(t *testing.T) {
	t.Setenv("GOGC", "")
	t.Setenv(adaptiveGCDisableEnv, "")
	orig := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(orig); gcTightening.Store(false) })

	state := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}

	// A heap spike tightens (subtick path: host-blind, cannot release).
	gcGovernor(0.95, -1, 0, false, state)
	if state.level != 3 {
		t.Fatalf("spike should reach level 3, got %d", state.level)
	}

	// Calm samples on the self-heal-off tick must walk the level back to 0.
	var logs strings.Builder
	for i := 0; i < 4*3; i++ {
		logs.WriteString(captureTlog(t, func() { gcSelfHealOffTick(0.10, 4000, state) }))
	}
	if state.level != 0 {
		t.Fatalf("12 calm self-heal-off ticks should release to level 0, stuck at %d", state.level)
	}
	if state.currentGOGC != 100 {
		t.Fatalf("release must restore the baseline GOGC, got %d", state.currentGOGC)
	}
	if !strings.Contains(logs.String(), "[proxy][pressure] gcGovernor") {
		t.Fatalf("level changes must be logged, got:\n%s", logs.String())
	}
	if gcTightening.Load() {
		t.Fatalf("gcTightening must clear once released")
	}
}

// The subtick tighten must log too; it used to be silent, so an operator could
// not tell why GOGC had dropped.
func TestGCSubtickTightenIsLogged(t *testing.T) {
	t.Setenv("GOGC", "")
	t.Setenv(adaptiveGCDisableEnv, "")
	orig := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(orig); gcTightening.Store(false) })

	state := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	out := captureTlog(t, func() { gcSubtickStep(0.85, state) })
	if state.level != 2 {
		t.Fatalf("0.85 heap should reach level 2, got %d", state.level)
	}
	if !strings.Contains(out, "[proxy][pressure] gcGovernor hard_gogc25") {
		t.Fatalf("subtick tighten not logged, got:\n%s", out)
	}
}
