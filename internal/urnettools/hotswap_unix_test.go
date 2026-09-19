//go:build linux

// Tests for HotSwap behaviour that only exists on non-Windows builds.
//
// ErrHotSwapUnitNotNotify is declared in hotswap_unix.go, which is
// //go:build !windows. Keeping a test that references it in the untagged
// hotswap_test.go made the whole urnettools package fail to COMPILE under
// GOOS=windows, not just fail a test, which is why the Windows lifecycle job
// went red on a build error rather than an assertion.
//
// tag=linux (not !windows): the unit-notify gate is exercised through the
// running-image version fixture (startSwappedFakeProvider), which swaps a
// binary out from under a live process via /proc/<pid>/exe — kernel
// behaviour that only exists on Linux. Darwin compiles hotswap_unix.go but
// has no /proc swap semantics, so this test is linux-only.

package urnettools

import (
	"errors"
	"testing"
)

// TestTriggerHotSwapUnitNotNotify: a supported version on a Type=simple
// unit must fail with the unit-specific error (naming Type=notify and how
// to fix it), not the generic version-mismatch error — an operator reading
// "requires >= v3.23.0-fix.31.0" on an already-current binary would chase
// the wrong fix entirely.
func TestTriggerHotSwapUnitNotNotify(t *testing.T) {
	origUnitType := unitTypeFunc
	defer func() { unitTypeFunc = origUnitType }()
	unitTypeFunc = func(Provider) (string, error) { return "simple", nil }

	// A real child provider carrying the v31.0 version stamp: with the
	// read-only version resolution, hotSwapVersionOK reads the RUNNING
	// image's stamp. Using os.Getpid() here would resolve the TEST binary's
	// own embedded stamp (v3.23.0-fix.26.4) and fail the version gate for
	// the wrong reason — this fixture makes the version gate pass so the
	// test exercises the unit gate, which is its point.
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.31.0")
	p := Provider{
		PID:     pid,
		Version: "v3.23.0-fix.31.0",
		Unit:    "urnetwork.service",
		Running: true,
	}
	err := triggerHotSwap(p)
	if !errors.Is(err, ErrHotSwapUnitNotNotify) {
		t.Fatalf("triggerHotSwap on a Type=simple unit = %v, want ErrHotSwapUnitNotNotify", err)
	}
}
