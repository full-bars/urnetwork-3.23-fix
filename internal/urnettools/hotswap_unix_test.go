//go:build !windows

// Tests for HotSwap behaviour that only exists on non-Windows builds.
//
// ErrHotSwapUnitNotNotify is declared in hotswap_unix.go, which is
// //go:build !windows. Keeping a test that references it in the untagged
// hotswap_test.go made the whole urnettools package fail to COMPILE under
// GOOS=windows, not just fail a test, which is why the Windows lifecycle job
// went red on a build error rather than an assertion.

package urnettools

import (
	"errors"
	"os"
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

	p := Provider{
		PID:     os.Getpid(),
		Version: "v3.23.0-fix.31.0",
		Unit:    "urnetwork.service",
		Running: true,
	}
	err := triggerHotSwap(p)
	if !errors.Is(err, ErrHotSwapUnitNotNotify) {
		t.Fatalf("triggerHotSwap on a Type=simple unit = %v, want ErrHotSwapUnitNotNotify", err)
	}
}
