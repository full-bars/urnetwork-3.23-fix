package main

import (
	"strings"
	"testing"
	"time"
)

func TestLivenessGate(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	if ok, _ := livenessGate(true); !ok {
		t.Fatal("self-heal on and a fresh budget: the gate is open")
	}

	t.Setenv("URNETWORK_SELF_HEAL", "")
	if ok, why := livenessGate(true); ok || !strings.Contains(why, "self-heal is off") {
		t.Fatalf("self-heal off closes the gate, got ok=%v %q", ok, why)
	}
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	// spend the daily ring: three recent restarts
	now := time.Now()
	for i := 0; i < thrashMaxRestarts24h; i++ {
		if err := recordThrashEscalation(0, now.Add(-time.Duration(i+1)*3*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if ok, why := livenessGate(true); ok || !strings.Contains(why, "restart budget is spent") {
		t.Fatalf("a spent ring closes the gate at the start of an episode, got ok=%v %q", ok, why)
	}
	if ok, _ := livenessGate(false); !ok {
		t.Fatal("mid-episode the budget is not asked again (it includes the episode's own entry)")
	}
}

func TestRecordLivenessStallKeepsAnExistingCapInsteadOfCompounding(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	recordLivenessStall()
	if cap, ok := activeThrashCap(time.Now()); !ok || cap != 300 {
		t.Fatalf("first stall: cap 300 expected, got %d ok=%v", cap, ok)
	}
	// the node restarts with the cap: it now runs 300; a second stall must not cut 60% of that
	lastRunningProxyCount.Store(300)
	time.Sleep(1100 * time.Millisecond) // a distinct ring timestamp
	recordLivenessStall()
	if cap, _ := activeThrashCap(time.Now()); cap != 300 {
		t.Fatalf("a stall under a standing cap must keep it, not compound to %d", cap)
	}
}

func TestRecordLivenessStallUndoRemovesOnlyItsOwnEntry(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	// a restart recorded earlier by a real thrash escape (and a cap of 200)
	earlier := time.Now().Add(-2 * time.Hour)
	if err := recordThrashEscalation(200, earlier); err != nil {
		t.Fatal(err)
	}
	before := readThrashCapState()

	undo := recordLivenessStall()
	mid := readThrashCapState()
	if len(mid.Restarts) != len(before.Restarts)+1 {
		t.Fatalf("the stall must add one ring entry, %d -> %d", len(before.Restarts), len(mid.Restarts))
	}
	// another writer adds its own entry before the undo runs
	other := time.Now().Add(time.Second)
	if err := recordThrashEscalation(0, other); err != nil {
		t.Fatal(err)
	}

	undo()

	after := readThrashCapState()
	hasEarlier, hasOther := false, false
	for _, ts := range after.Restarts {
		if ts == earlier.Unix() {
			hasEarlier = true
		}
		if ts == other.Unix() {
			hasOther = true
		}
	}
	if !hasEarlier || !hasOther {
		t.Fatalf("the undo must keep entries it did not add: %v", after.Restarts)
	}
	if len(after.Restarts) != len(before.Restarts)+1 {
		t.Fatalf("exactly the stall's own entry must be gone, ring %v", after.Restarts)
	}
	if after.Cap != 200 {
		t.Fatalf("the earlier cap must be back, got %d", after.Cap)
	}
}
