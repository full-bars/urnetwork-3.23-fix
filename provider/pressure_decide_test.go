package main

import (
	"math"
	"testing"
)

// Golden table for pressureDecide. Every expected value is the captured
// output of the pre-extraction decision block of runPressureMonitor over the
// same rows, or derived and hand-verified against it (the saturated-psi-cpu
// and steady-zero rows); the float literals are the captured shortest
// round-trip values and are compared with a small tolerance, so any change in
// the smoothing, the emergency pins or the regime buckets fails here.
func TestPressureDecide(t *testing.T) {
	tests := []struct {
		name         string
		prev         pressureState
		sample       pressureSample
		wantSmoothed float64
		wantRegime   int
		wantNoCPU    float64
		wantChanged  bool
	}{
		// Rise from zero: alpha 0.5 on the way up.
		{"rise-from-zero", pressureState{smoothed: 0, smoothedNoCPU: 0, lastRegime: 0}, pressureSample{PSIMem: 30}, 0.2, 0, 0.2, false},
		// Decay: alpha 0.1 on the way down.
		{"decay-slower-than-rise", pressureState{smoothed: 0.7, smoothedNoCPU: 0.6, lastRegime: 2}, pressureSample{}, 0.63, 2, 0.54, false},
		// Emergency heap pin bypasses smoothing; the companion follows
		// through its own saturated component.
		{"emergency-heap-pin", pressureState{smoothed: 0.2, smoothedNoCPU: 0.1, lastRegime: 0}, pressureSample{HeapFrac: 0.95}, 1, 3, 1, true},
		{"emergency-goroutine-pin", pressureState{smoothed: 0.3, smoothedNoCPU: 0.2, lastRegime: 1}, pressureSample{Goroutines: 30000}, 1, 3, 1, true},
		// CPU pressure lifts the main score but not the CPU-excluded
		// companion (the pool grow gate).
		{"cpu-only-pushes-score-not-nocpu", pressureState{smoothed: 0.5, smoothedNoCPU: 0.5, lastRegime: 2}, pressureSample{PSICPU: 55, Cores: 8}, 0.7, 2, 0.45, false},
		// Crossing into the next regime bucket reports the change.
		{"regime-cross-up", pressureState{smoothed: 0.74, smoothedNoCPU: 0.7, lastRegime: 2}, pressureSample{PSIMem: 48}, 0.75, 3, 0.73, true},
		// The single-core CPU ramp differs from the multi-core PSI ramp.
		{"single-core-cpu-ramp", pressureState{smoothed: 0, smoothedNoCPU: 0, lastRegime: 0}, pressureSample{PSICPU: 65, Cores: 1}, 0.25, 1, 0, true},
		// A component at its ramp ceiling pins the raw score to 1.0.
		{"psi-mem-saturates-pins", pressureState{smoothed: 0.1, smoothedNoCPU: 0.1, lastRegime: 1}, pressureSample{PSIMem: 60}, 1, 3, 1, true},
		// Decay inside the same regime reports no change.
		{"decay-unchanged-regime", pressureState{smoothed: 0.6, smoothedNoCPU: 0.55, lastRegime: 2}, pressureSample{PSIMem: 15}, 0.5499999999999999, 2, 0.505, false},
		// Saturated psi_cpu alone pins the main score to 1.0 while the
		// CPU-excluded companion keeps decaying toward zero (the scoring
		// fix: a cpu-only blowout must not block pool growth).
		{"saturated-psi-cpu-only", pressureState{smoothed: 0.3, smoothedNoCPU: 0.2, lastRegime: 0}, pressureSample{PSICPU: 100, Cores: 8}, 1, 3, 0.18, true},
		// A zero sample on a zero prev is the steady state: no regime change.
		{"steady-zero", pressureState{}, pressureSample{}, 0, 0, 0, false},
	}

	for _, tt := range tests {
		smoothed, regime, acts := pressureDecide(tt.prev, tt.sample)
		// Floats compare with a tolerance: on arm64/ppc64le/s390x the
		// compiler may fuse the EWMA multiply-add, moving the last ulp.
		// Regime and the change flag stay exact.
		if math.Abs(smoothed-tt.wantSmoothed) > 1e-12 || regime != tt.wantRegime ||
			math.Abs(acts.publishNoCPU-tt.wantNoCPU) > 1e-12 || acts.regimeChanged != tt.wantChanged {
			t.Errorf("%s: pressureDecide() = (smoothed=%v, regime=%d, noCPU=%v, changed=%v), want (%v, %d, %v, %v)",
				tt.name, smoothed, regime, acts.publishNoCPU, acts.regimeChanged,
				tt.wantSmoothed, tt.wantRegime, tt.wantNoCPU, tt.wantChanged)
		}
		if acts.comps == nil {
			t.Errorf("%s: actions.comps must carry the component breakdown the loop feeds to the GC veto, the status file and the log line", tt.name)
			continue
		}
		for _, key := range []string{"psi_mem", "psi_cpu", "psi_io", "load", "goro"} {
			if _, ok := acts.comps[key]; !ok {
				t.Errorf("%s: actions.comps missing %q (the loop reads these keys)", tt.name, key)
			}
		}
	}
}
