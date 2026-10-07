package main

import "testing"

func TestMemPressureMacLevelMapping(t *testing.T) {
	normal := memPressurePercentFromMacLevel(macPressureNormal)
	warn := memPressurePercentFromMacLevel(macPressureWarn)
	crit := memPressurePercentFromMacLevel(macPressureCritical)
	if normalizeRamp(normal, psiRampLo, psiRampHi) != 0 {
		t.Errorf("normal must score 0, got percent %v", normal)
	}
	if s := normalizeRamp(warn, psiRampLo, psiRampHi); s <= 0 || s >= 1 {
		t.Errorf("warn must score strictly between 0 and 1, got %v", s)
	}
	if s := normalizeRamp(crit, psiRampLo, psiRampHi); s != 1 {
		t.Errorf("critical must pin the component at 1, got %v", s)
	}
	for _, level := range []uint32{0, 3, 8, 255} {
		if v := memPressurePercentFromMacLevel(level); v != 0 {
			t.Errorf("unknown level %d must fail open, got %v", level, v)
		}
	}
}

func TestMemPressureWindowsLowMapping(t *testing.T) {
	if memPressurePercentWindowsLow(false) != 0 {
		t.Error("not low must report 0")
	}
	if s := normalizeRamp(memPressurePercentWindowsLow(true), psiRampLo, psiRampHi); s != 1 {
		t.Errorf("low must pin psi_mem at 1, got %v", s)
	}
}

// Off Linux the host reader supplies PSIMem; the pure scoring must treat a
// level-derived percent exactly like a PSI one.
func TestPSIMemFromHostLevelScores(t *testing.T) {
	_, comps := computePressure(pressureSample{PSIMem: memPressurePercentFromMacLevel(macPressureWarn)})
	if comps["psi_mem"] <= 0 {
		t.Fatalf("warn level should raise psi_mem, comps=%v", comps)
	}
}

func TestMemPressureSwapFracMapping(t *testing.T) {
	for _, frac := range []float64{0, -0.5, -1} {
		if v := memPressurePercentFromSwapFrac(frac); v != 0 {
			t.Errorf("no swap in use (frac %v) must report 0, got %v", frac, v)
		}
	}
	// A quarter of the swap device in use already scores at the top of the
	// ramp: swap use is lagging, so a modest figure still means real pressure.
	if s := normalizeRamp(memPressurePercentFromSwapFrac(swapFracRampFull), psiRampLo, psiRampHi); s != 1 {
		t.Errorf("frac %v must pin psi_mem at 1, got %v", swapFracRampFull, s)
	}
	// Halfway to that point must land strictly inside the ramp, not at either
	// end, or the component would snap from 0 to 1 with no middle.
	mid := swapFracRampFull / 2
	s := normalizeRamp(memPressurePercentFromSwapFrac(mid), psiRampLo, psiRampHi)
	if s <= 0 || s >= 1 {
		t.Errorf("frac %v must score strictly between 0 and 1, got %v", mid, s)
	}
	// Beyond the device size the reading is meaningless; clamp rather than
	// extrapolate past the top of the ramp.
	for _, frac := range []float64{1, 1.5, 100} {
		if v := memPressurePercentFromSwapFrac(frac); v != psiRampHi {
			t.Errorf("frac %v must clamp to psiRampHi (%v), got %v", frac, psiRampHi, v)
		}
	}
	// The swap-derived percent must feed the component the same way a PSI
	// reading does, since that is the path the collector takes when PSI is
	// unavailable.
	if _, comps := computePressure(pressureSample{PSIMem: memPressurePercentFromSwapFrac(mid)}); comps["psi_mem"] <= 0 {
		t.Errorf("swap-derived percent should raise psi_mem, comps=%v", comps)
	}
}
