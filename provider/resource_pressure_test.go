package main

import (
	"math"
	"testing"
)

func almostEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNormalizeRamp(t *testing.T) {
	// (value, lo, hi) → 0 below lo, 1 above hi, linear between
	if v := normalizeRamp(5, 10, 60); v != 0 {
		t.Fatalf("below lo: got %v", v)
	}
	if v := normalizeRamp(80, 10, 60); v != 1 {
		t.Fatalf("above hi: got %v", v)
	}
	if v := normalizeRamp(35, 10, 60); !almostEq(v, 0.5) {
		t.Fatalf("midpoint: got %v", v)
	}
}

func TestComputePressure_WorstComponentWins(t *testing.T) {
	s := pressureSample{
		PSIMem:       35,   // → 0.5 on the 10..60 ramp
		PSICPU:       10,   // → 0
		MemAvailFrac: 0.30, // healthy → 0
		LoadPerCore:  0.5,  // → 0
		Goroutines:   1000, // → 0
		HeapFrac:     0.2,  // → 0
	}
	score, comps := computePressure(s)
	if !almostEq(score, 0.5) {
		t.Fatalf("expected max()=0.5 from psi_mem, got %v (comps=%v)", score, comps)
	}
}

func TestComputePressure_EmergencyPin(t *testing.T) {
	s := pressureSample{HeapFrac: 0.95}
	if score, _ := computePressure(s); score != 1.0 {
		t.Fatalf("heap >90%% of limit must pin to 1.0, got %v", score)
	}
	s = pressureSample{Goroutines: 30000}
	if score, _ := computePressure(s); score != 1.0 {
		t.Fatalf("goroutine blowout must pin to 1.0, got %v", score)
	}
}

func TestComputePressure_MissingSensorsFailOpen(t *testing.T) {
	// zero-value sample (all sensors errored / non-Linux): score must be 0
	if score, _ := computePressure(pressureSample{}); score != 0 {
		t.Fatalf("no data must mean no pressure, got %v", score)
	}
}

func TestEwmaAsymmetric(t *testing.T) {
	// rise fast: prev 0, raw 1 → 0.5 (alpha 0.5)
	if v := ewmaUpdate(0, 1); !almostEq(v, 0.5) {
		t.Fatalf("rise: got %v", v)
	}
	// decay slow: prev 1, raw 0 → 0.9 (alpha 0.1)
	if v := ewmaUpdate(1, 0); !almostEq(v, 0.9) {
		t.Fatalf("decay: got %v", v)
	}
}

func TestParsePSILine(t *testing.T) {
	avg10, avg60, err := parsePSISome("some avg10=12.34 avg60=5.60 avg300=1.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	if err != nil || !almostEq(avg10, 12.34) || !almostEq(avg60, 5.60) {
		t.Fatalf("got %v %v %v", avg10, avg60, err)
	}
}
