package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pressure monitor and the reload loop both write events.log. The open,
// stat, rotate and write sequence must run under critLogMu, or two callers can
// both rotate and lose or interleave lines. Asserted directly from inside the
// critical section, so it does not depend on goroutine timing.
func TestCritLogRunsItsSequenceUnderTheMutex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var held, ran bool
	critLogInCriticalSection = func() {
		ran = true
		// TryLock fails only if another holder, here critLog itself, has it.
		if critLogMu.TryLock() {
			critLogMu.Unlock()
			return
		}
		held = true
	}
	t.Cleanup(func() { critLogInCriticalSection = nil })

	critLog("[test] hello")
	if !ran {
		t.Fatal("the critical-section hook never ran")
	}
	if !held {
		t.Fatal("critLog did not hold critLogMu while it opened, rotated and wrote")
	}
	if critLogMu.TryLock() {
		critLogMu.Unlock()
	} else {
		t.Fatal("critLog returned without releasing critLogMu")
	}
}

// Crossing the size limit rotates once, keeps only whole lines and leaves the
// file bounded, with the newest line last. Single goroutine, so it is exact.
func TestCritLogRotatesAtTheLimitKeepingWholeLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	payload := strings.Repeat("x", 2000)
	const n = 700 // about 1.4 MB, past the 1 MiB limit exactly once
	for i := 0; i < n; i++ {
		critLog("[test] line=%d %s", i, payload)
	}
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || len(b) > critLogMaxSize+4096 {
		t.Fatalf("events.log size %d, want within (0, %d]", len(b), critLogMaxSize+4096)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) >= n {
		t.Fatalf("no rotation happened: %d lines kept of %d written", len(lines), n)
	}
	for i, line := range lines {
		if !strings.Contains(line, "[test] line=") || !strings.HasSuffix(line, payload) {
			t.Fatalf("line %d is torn: %.80q (len %d)", i, line, len(line))
		}
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "line=699 ") {
		t.Fatalf("the newest line must survive rotation, last = %.60q", last)
	}
}
