package main

import (
	"fmt"
	"math"
	"runtime/debug"
	"strings"
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

// A live side effect that wedges a node must name itself in the log. The
// transition is already logged ("cleared gomemlimit (was 2GiB)"), but that
// says what was removed, not what the runtime now holds. An operator whose
// node pegs after a clear had nothing tying the symptom to the setting.
func TestApplyLiveSideEffectLogsTheEffectiveValue(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	var logged []string
	restore := captureControlApplyLog(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	t.Cleanup(restore)

	if err := applyLiveSideEffect("gomemlimit", "0"); err != nil {
		t.Fatalf("applyLiveSideEffect: %v", err)
	}

	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "gomemlimit") {
		t.Errorf("apply did not name the key in the log; got %q", joined)
	}
	if !strings.Contains(joined, "unlimited") {
		t.Errorf("apply did not report the effective value; got %q", joined)
	}
}

// captureControlApplyLog redirects the apply log for the duration of a test.
func captureControlApplyLog(fn func(string, ...any)) func() {
	orig := controlApplyLog
	controlApplyLog = fn
	return func() { controlApplyLog = orig }
}

// `off` means clear for every tuning key, so it cannot also mean "disable
// the collector" for gogc without making the dangerous reading the default
// one. `disabled` is the explicit, self-describing value that reaches
// SetGCPercent(-1), so nobody gets an unbounded heap by typing the word
// every other key uses for "remove this".
func TestGogcDisabledIsAcceptedAndDisablesCollection(t *testing.T) {
	origGC := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(origGC) })

	if err := validateControlValue("gogc", "disabled"); err != nil {
		t.Fatalf("validateControlValue(gogc, disabled) = %v, want accepted", err)
	}

	var logged []string
	restore := captureControlApplyLog(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	t.Cleanup(restore)

	if err := applyLiveSideEffect("gogc", "disabled"); err != nil {
		t.Fatalf("applyLiveSideEffect(gogc, disabled): %v", err)
	}

	// SetGCPercent returns the previous value; -1 means collection was off.
	if prev := debug.SetGCPercent(100); prev != -1 {
		t.Errorf("gogc after 'disabled' = %d, want -1 (collection disabled)", prev)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "disabled") {
		t.Errorf("apply did not announce that collection was disabled; got %q", logged)
	}
}
