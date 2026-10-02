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
