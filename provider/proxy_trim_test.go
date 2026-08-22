package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelectWorstRunningProxies pins the A-F-worst shed order: dead first, then
// ungraded, then graded by ascending reachability score, then lower traffic.
func TestSelectWorstRunningProxies(t *testing.T) {
	state := map[string]ProxyEntry{
		"dead:1":   {Health: "dead"},
		"f:1":      {Health: "up", Score: 0.1, Graded: true}, // worst grade
		"a:1":      {Health: "up", Score: 0.9, Graded: true}, // best grade
		"c:1":      {Health: "up", Score: 0.5, Graded: true},
		"ungrad:1": {Health: "up"}, // never graded -> shed early
	}
	running := []string{"dead:1", "f:1", "a:1", "c:1", "ungrad:1"}
	traffic := map[string]uint64{"a:1": 500, "f:1": 1000, "c:1": 700}
	// Shed 1: dead first.
	if got := selectWorstRunningProxies(state, nil, traffic, running, 1); len(got) != 1 || got[0] != "dead:1" {
		t.Fatalf("shed 1 = %v, want [dead:1]", got)
	}
	// Shed 3: dead, then ungraded, then worst grade (f:1 score 0.1 < c:1 0.5).
	got := selectWorstRunningProxies(state, nil, traffic, running, 3)
	want := []string{"dead:1", "ungrad:1", "f:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shed 3[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
	// Tiebreak by traffic: c:1 (700) sheds before a:1 (500) only when both are
	// the same grade — here scores differ, so this just asserts order.
}

// TestSelectWorstTrafficTiebreak: same grade, lower traffic sheds first.
func TestSelectWorstTrafficTiebreak(t *testing.T) {
	state := map[string]ProxyEntry{
		"low:1":  {Health: "up", Score: 0.5, Graded: true},
		"high:1": {Health: "up", Score: 0.5, Graded: true},
	}
	traffic := map[string]uint64{"low:1": 100, "high:1": 1000}
	got := selectWorstRunningProxies(state, nil, traffic, []string{"high:1", "low:1"}, 1)
	if got[0] != "low:1" {
		t.Fatalf("traffic tiebreak shed = %v, want [low:1]", got)
	}
}

// TestReadWriteTrimTarget covers set / off / clear round-trip via $HOME override.
func TestReadWriteTrimTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	path := filepath.Join(home, ".urnetwork", "proxy_trim")

	if n, err := readTrimTarget(); err != nil || n != 0 {
		t.Fatalf("initial readTrimTarget = %d,%v; want 0", n, err)
	}
	if err := writeTrimTarget(500); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n, _ := readTrimTarget(); n != 500 {
		t.Fatalf("after set readTrimTarget = %d, want 500", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("target file missing: %v", err)
	}
	// off clears.
	if err := writeTrimTarget(0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target file should be removed on clear, err=%v", err)
	}
	defer os.Remove(path)
}
