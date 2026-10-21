package main

import "testing"

// TestAddProxyDeltasSkipsFirstSight pins the reason the helper exists: a proxy
// that appears with a seeded counter must not have its whole lifetime total
// counted as one tick of traffic.
func TestAddProxyDeltasSkipsFirstSight(t *testing.T) {
	prev := map[string]uint64{}
	cur := map[string]uint64{"a": 1 << 30, "b": 10}
	if got := addProxyDeltas(prev, cur); got != 0 {
		t.Fatalf("first sight delta = %d, want 0", got)
	}
	if prev["a"] != 1<<30 || prev["b"] != 10 {
		t.Fatalf("baselines not recorded: %v", prev)
	}
}

func TestAddProxyDeltasCountsMovement(t *testing.T) {
	prev := map[string]uint64{"a": 100, "b": 50}
	cur := map[string]uint64{"a": 160, "b": 50}
	if got := addProxyDeltas(prev, cur); got != 60 {
		t.Fatalf("delta = %d, want 60", got)
	}
}

// TestAddProxyDeltasIgnoresBackwardsCounter: a reset re-baselines without
// contributing a negative, and the next move counts from the new baseline.
func TestAddProxyDeltasIgnoresBackwardsCounter(t *testing.T) {
	prev := map[string]uint64{"a": 500}
	if got := addProxyDeltas(prev, map[string]uint64{"a": 20}); got != 0 {
		t.Fatalf("backwards delta = %d, want 0", got)
	}
	if prev["a"] != 20 {
		t.Fatalf("baseline = %d, want 20", prev["a"])
	}
	if got := addProxyDeltas(prev, map[string]uint64{"a": 30}); got != 10 {
		t.Fatalf("delta after reset = %d, want 10", got)
	}
}

// TestAddProxyDeltasPrunesDepartedKeys: a proxy that left the snapshot must not
// leave a baseline behind, or a returning proxy would delta against stale
// history the helper exists to ignore.
func TestAddProxyDeltasPrunesDepartedKeys(t *testing.T) {
	prev := map[string]uint64{"a": 100, "gone": 900}
	addProxyDeltas(prev, map[string]uint64{"a": 110})
	if _, ok := prev["gone"]; ok {
		t.Fatalf("departed key kept: %v", prev)
	}
}
