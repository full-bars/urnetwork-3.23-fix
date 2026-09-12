package main

import (
	"math"
	"runtime/debug"
	"testing"
)

// `urnet-tools clear gomemlimit` reapplies the live default, which was "0".
// In Go, zero is a real zero-byte soft limit, not "unlimited": the runtime
// then sees every allocation as over budget and runs GC continuously,
// pegging the CPU until the process is killed. math.MaxInt64 is the value
// that means unlimited.
func TestClearGomemlimitRestoresUnlimitedNotZero(t *testing.T) {
	// SetMemoryLimit(-1) reports the current limit without changing it.
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	if err := applyLiveDefault("gomemlimit"); err != nil {
		t.Fatalf("applyLiveDefault(gomemlimit): %v", err)
	}

	got := debug.SetMemoryLimit(-1)
	if got == 0 {
		t.Fatal("clearing gomemlimit set a zero-byte memory limit; the runtime would GC continuously and peg the CPU")
	}
	if got != math.MaxInt64 {
		t.Errorf("memory limit after clear = %d, want math.MaxInt64 (unlimited)", got)
	}
}
