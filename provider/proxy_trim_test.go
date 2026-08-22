package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelectWorstRunningProxies pins the shed order: dead first, then confirmed F-tier
// (score 0.1 < 0.595), then ungraded (0.595), then C-tier (0.7), then A-tier (0.9).
func TestSelectWorstRunningProxies(t *testing.T) {
	state := map[string]ProxyEntry{
		"dead:1":   {Health: "dead"},
		"f:1":      {Health: "up", Score: 0.1, Graded: true}, // confirmed failing F-grade
		"a:1":      {Health: "up", Score: 0.9, Graded: true}, // best grade
		"c:1":      {Health: "up", Score: 0.7, Graded: true},
		"ungrad:1": {Health: "up"}, // never graded (probationary)
	}
	running := []string{"dead:1", "f:1", "a:1", "c:1", "ungrad:1"}
	// All idle (0 traffic) -> test reputation ranking
	traffic := map[string]uint64{}

	// Shed 1: dead first.
	if got := selectWorstRunningProxies(state, nil, traffic, running, 1); len(got) != 1 || got[0] != "dead:1" {
		t.Fatalf("shed 1 = %v, want [dead:1]", got)
	}
	// Shed 3: dead, then confirmed F-tier (score 0.1), then ungraded (0.595).
	got := selectWorstRunningProxies(state, nil, traffic, running, 3)
	want := []string{"dead:1", "f:1", "ungrad:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shed 3[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestSelectWorstEarningProtection: an active earner (even with a low synthetic
// grade, e.g. D or F) must NEVER be shed before an idle proxy with 0 traffic.
func TestSelectWorstEarningProtection(t *testing.T) {
	state := map[string]ProxyEntry{
		"idle_a:1":   {Health: "up", Score: 0.95, Graded: true}, // A-tier but 0 bytes moved
		"idle_un:1":  {Health: "up"},                            // Ungraded, 0 bytes moved
		"earner_f:1": {Health: "up", Score: 0.1, Graded: true},  // F-tier synthetic grade BUT moving 50 MB
	}
	traffic := map[string]uint64{
		"idle_a:1":   0,
		"idle_un:1":  0,
		"earner_f:1": 50 * 1024 * 1024,
	}
	running := []string{"idle_a:1", "idle_un:1", "earner_f:1"}

	// Shed 2: both idle proxies must shed BEFORE the earning proxy, even though
	// idle_a:1 has an A-grade synthetic score.
	got := selectWorstRunningProxies(state, nil, traffic, running, 2)
	want := []string{"idle_un:1", "idle_a:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shed 2[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestSelectWorstTrafficScale: among active earners, smaller earners shed before
// larger earners to preserve maximum operator revenue.
func TestSelectWorstTrafficScale(t *testing.T) {
	state := map[string]ProxyEntry{
		"small:1": {Health: "up", Score: 0.9, Graded: true},
		"huge:1":  {Health: "up", Score: 0.7, Graded: true},
	}
	traffic := map[string]uint64{
		"small:1": 1024,
		"huge:1":  500 * 1024 * 1024,
	}
	got := selectWorstRunningProxies(state, nil, traffic, []string{"huge:1", "small:1"}, 1)
	if got[0] != "small:1" {
		t.Fatalf("traffic scale shed = %v, want [small:1]", got)
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
