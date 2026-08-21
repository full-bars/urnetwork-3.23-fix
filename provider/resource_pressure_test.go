package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"testing"
	"time"
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

func TestNormalizeRamp_EqualAnchors(t *testing.T) {
	// lo == hi → returns 0 (degenerate, can't map a range of zero width)
	if v := normalizeRamp(42, 10, 10); v != 0 {
		t.Fatalf("equal anchors: got %v", v)
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

func TestScaledProbeConcurrency(t *testing.T) {
	if v := scaledProbeConcurrency(0); v != proxyProbeConcurrency {
		t.Fatalf("calm: %v", v)
	}
	if v := scaledProbeConcurrency(0.5); v != proxyProbeConcurrency/2 {
		t.Fatalf("half: %v", v)
	}
	if v := scaledProbeConcurrency(1.0); v != 1 {
		t.Fatalf("floor: %v", v)
	}
}

func TestParsePSILine(t *testing.T) {
	avg10, avg60, err := parsePSISome("some avg10=12.34 avg60=5.60 avg300=1.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	if err != nil || !almostEq(avg10, 12.34) || !almostEq(avg60, 5.60) {
		t.Fatalf("got %v %v %v", avg10, avg60, err)
	}
}

func TestParsePSILine_NoSomeLine(t *testing.T) {
	_, _, err := parsePSISome("full avg10=0.00 avg60=0.00\n")
	if err == nil {
		t.Fatalf("expected error for missing 'some' line")
	}
}

func TestCleanupIntervalScale(t *testing.T) {
	if v := cleanupIntervalScale(0); !almostEq(v, 1.0) {
		t.Fatalf("calm: %v", v)
	}
	if v := cleanupIntervalScale(0.8); !almostEq(v, 1.0/6.0) {
		t.Fatalf("high: %v", v)
	}
	if v := cleanupIntervalScale(1.0); !almostEq(v, 1.0/6.0) {
		t.Fatalf("pinned: %v", v)
	}
}

func TestReaperStaleThreshold(t *testing.T) {
	if v := reaperStaleThreshold(0); v != 3*time.Hour {
		t.Fatalf("calm: %v", v)
	}
	if v := reaperStaleThreshold(0.9); v != time.Hour {
		t.Fatalf("high: %v", v)
	}
}

// TestPaidStaleThreshold pins the PAID/file-proxy stale window (6h calm /
// 3h hot), deliberately 3x wider than the URL window (reaperStaleThreshold)
// since the operator pays for paid-probe bandwidth and paid proxies are
// stable by construction.
func TestPaidStaleThreshold(t *testing.T) {
	if v := paidStaleThreshold(0); v != 6*time.Hour {
		t.Fatalf("calm: %v", v)
	}
	if v := paidStaleThreshold(0.9); v != 3*time.Hour {
		t.Fatalf("high: %v", v)
	}
	// Same ramp anchors as reaperStaleThreshold (cleanupScaleStart/Full):
	// below the start anchor stays fully calm.
	if v := paidStaleThreshold(cleanupScaleStart); v != 6*time.Hour {
		t.Fatalf("at ramp start: %v", v)
	}
	// Midpoint of the ramp: halfway between 6h and 3h.
	mid := cleanupScaleStart + (cleanupScaleFull-cleanupScaleStart)/2
	if v := paidStaleThreshold(mid); v != 6*time.Hour-90*time.Minute {
		t.Fatalf("midpoint: %v", v)
	}
	// The paid window must always be >= the URL window at the same
	// pressure, or the paid/free divergence this feature exists for is
	// broken.
	for _, p := range []float64{0, 0.3, 0.5, 0.8, 1.0} {
		if paidStaleThreshold(p) < reaperStaleThreshold(p) {
			t.Fatalf("pressure %.2f: paid window %v must be >= URL window %v",
				p, paidStaleThreshold(p), reaperStaleThreshold(p))
		}
	}
}

func TestAimdStep(t *testing.T) {
	// calm growth, capped by ceiling
	if v := aimdStep(100, 100, 0.1, 500); v != 125 {
		t.Fatalf("grow: %v", v)
	}
	if v := aimdStep(490, 490, 0.1, 500); v != 500 {
		t.Fatalf("ceiling: %v", v)
	}
	// ceiling 0 = unlimited growth allowed
	if v := aimdStep(1000, 1000, 0.1, 0); v != 1025 {
		t.Fatalf("unlimited: %v", v)
	}
	// sustained pressure: multiplicative decrease with floor
	if v := aimdStep(500, 500, 0.8, 500); v != 350 {
		t.Fatalf("cut: %v", v)
	}
	if v := aimdStep(60, 60, 0.8, 500); v != 50 {
		t.Fatalf("floor: %v", v)
	}
	// middle band: hold
	if v := aimdStep(200, 200, 0.5, 500); v != 200 {
		t.Fatalf("hold: %v", v)
	}
	// never grow past what's actually cached +25 headroom is fine, but
	// don't run target away from reality when cache is far below target.
	// NOTE: the brief's own snippet asserted 150 here, which contradicts
	// both its implementation (cacheSize+aimdIncrement = 100+25 = 125) and
	// its own inline comment ("track reality when cache lags target" —
	// i.e. cache+increment, not cache+2*increment). Fixed to 125 to match
	// the documented/implemented formula; flagged in the task report.
	if v := aimdStep(400, 100, 0.1, 500); v != 125 {
		t.Fatalf("target tracks cache+increment when cache lags: %v", v)
	}
}

func TestWritePressureStatus(t *testing.T) {
	home := withTempHome(t)
	gc := &gcGovernorState{level: 2, lastHeapFrac: 0.85, gcStateName: "hard"}
	writePressureStatus(0.42, map[string]float64{"psi_mem": 0.42}, gc)
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "pressure_status"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Score      float64            `json:"score"`
		Components map[string]float64 `json:"components"`
		GCState    string             `json:"gc_state"`
		HeapFrac   float64            `json:"heap_frac"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !almostEq(got.Score, 0.42) || !almostEq(got.Components["psi_mem"], 0.42) {
		t.Fatalf("got %+v", got)
	}
	if got.GCState != "hard" || !almostEq(got.HeapFrac, 0.85) {
		t.Fatalf("expected gc_state=hard heap_frac=0.85, got gc_state=%q heap_frac=%v", got.GCState, got.HeapFrac)
	}
}

func TestSelectURLProxiesToShed(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "dead", Source: "url"},
		"3.3.3.3:1080": {Health: "offline", Source: "url"},
		"4.4.4.4:1080": {Health: "up", Source: "url"},
		"5.5.5.5:1080": {Health: "dead", Source: "file"}, // never shed: not url
	}}
	traffic := map[string]uint64{"1.1.1.1:1080": 100, "4.4.4.4:1080": 5}
	got := selectURLProxiesToShed(state, traffic, 3)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080", "4.4.4.4:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// n larger than pool: return everything url-sourced, still ordered
	if got := selectURLProxiesToShed(state, traffic, 99); len(got) != 4 {
		t.Fatalf("overshoot: %v", got)
	}
}

func TestSelectURLProxiesToShed_EmptyState(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	got := selectURLProxiesToShed(state, nil, 5)
	if len(got) != 0 {
		t.Fatalf("empty state should return nothing, got %v", got)
	}
}

func TestSelectURLProxiesToShed_AllFileSourced(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "dead", Source: "file"},
		"2.2.2.2:1080": {Health: "dead", Source: "file"},
	}}
	got := selectURLProxiesToShed(state, nil, 10)
	if len(got) != 0 {
		t.Fatalf("all file-sourced should return nothing, got %v", got)
	}
}

func TestSelectURLProxiesToShed_TrafficTieBreaking(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	// same health, no traffic data → sorted by address (lexicographic)
	got := selectURLProxiesToShed(state, nil, 2)
	want := []string{"1.1.1.1:1080", "2.2.2.2:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("tie-break by address: got %v want %v", got, want)
	}
}

func TestSelectURLProxiesToShed_TrafficOrdering(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	// same health, different traffic → shed lowest-traffic first
	traffic := map[string]uint64{
		"1.1.1.1:1080": 1000,
		"2.2.2.2:1080": 5,
		"3.3.3.3:1080": 500,
	}
	got := selectURLProxiesToShed(state, traffic, 2)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("shed lowest-traffic first: got %v want %v", got, want)
	}
}

func TestSelectURLProxiesToShed_HealthTierOrdering(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "inactive", Source: "url"},
		"3.3.3.3:1080": {Health: "long_offline", Source: "url"},
		"4.4.4.4:1080": {Health: "recently_offline", Source: "url"},
		"5.5.5.5:1080": {Health: "dead", Source: "url"},
	}}
	got := selectURLProxiesToShed(state, nil, 5)
	want := []string{"5.5.5.5:1080", "2.2.2.2:1080", "3.3.3.3:1080", "4.4.4.4:1080", "1.1.1.1:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("health tier ordering: got %v want %v", got, want)
	}
}

func TestPressureRegime(t *testing.T) {
	tests := []struct {
		score float64
		want  int
	}{
		{0.0, 0},
		{0.24, 0},
		{0.25, 1},
		{0.49, 1},
		{0.5, 2},
		{0.74, 2},
		{0.75, 3},
		{1.0, 3},
	}
	for _, tt := range tests {
		if got := pressureRegime(tt.score); got != tt.want {
			t.Errorf("pressureRegime(%v) = %d, want %d", tt.score, got, tt.want)
		}
	}
}

func TestFormatComponents(t *testing.T) {
	comps := map[string]float64{"psi_mem": 0.5, "load": 0.3}
	s := formatComponents(comps)
	if s != "psi_mem=0.50 load=0.30" {
		t.Fatalf("got %q", s)
	}
	// empty map → empty string
	if s := formatComponents(map[string]float64{}); s != "" {
		t.Fatalf("empty: got %q", s)
	}
}

// readGOGCPercent must read the effective GOGC without disabling the GC. The
// old SetGCPercent(-1) idiom sets GOGC to -1 (GC disabled) as a side effect;
// this pins that the read returns a sane positive baseline and does not leave
// GOGC set to -1 (disabled).
func TestReadGOGCPercent_DoesNotDisableGC(t *testing.T) {
	gogc, ok := readGOGCPercent()
	if !ok {
		t.Fatal("expected GOGC percent to be readable")
	}
	if gogc <= 0 {
		t.Fatalf("expected a positive GOGC baseline, got %d (a value <= 0 would mean GC disabled)", gogc)
	}
	// Verify a follow-up read is stable: if the read mutated GOGC to -1, the
	// next read would report something inconsistent. A stable positive read
	// confirms no disable side effect.
	gogc2, _ := readGOGCPercent()
	if gogc2 != gogc {
		t.Fatalf("GOGC read not stable (got %d then %d); a read with a disable side-effect would not be", gogc, gogc2)
	}
}

// The 10s subtick (canRelease=false) observed calm must NOT reset an in-flight
// release streak that the full sweep (canRelease=true) is accumulating for a
// host-driven tighten. If the subtick clobbered it, release would be
// unreachable (the fix this pins).
func TestGCGovernor_SubtickDoesNotClobberReleaseStreak(t *testing.T) {
	// This test drives the governor which writes process-global GOGC and
	// gcTightening. Capture both and restore them no matter how the test ends,
	// so a failing/panic run cannot poison later tests in the same package.
	origGoGC := 100
	if goc, ok := readGOGCPercent(); ok {
		origGoGC = goc
	}
	origTightening := gcTightening.Load()
	t.Cleanup(func() {
		debug.SetGCPercent(origGoGC)
		gcTightening.Store(origTightening)
	})

	var st gcGovernorState
	st.baselineGOGC = 100
	st.currentGOGC = 100

	// Full sweep raises to level 2 (hard) via the host signal (hostAvail=200MiB),
	// heap calm (0.5). This is a host-driven tighten.
	gcGovernor(0.50, 200, 0.1, true, &st) // full sweep: host 200 -> level 2
	if st.level != 2 {
		t.Fatalf("expected host-driven tighten to level 2, got %d", st.level)
	}

	// Between full sweeps there are subticks. The subtick is host-blind (-1) with
	// calm heap; it must not reset the streak (must not reach the else that zeroes
	// consecutiveCalmCount).
	before := st.consecutiveCalmCount
	gcGovernor(0.50, -1, 0, false, &st) // subtick: calm heap, no host signal, canRelease=false
	if st.consecutiveCalmCount < before {
		t.Fatalf("subtick clobbered calm streak (%d -> %d); release would be unreachable", before, st.consecutiveCalmCount)
	}

	// Now the full sweep sees the SAME calm (host recovered to 200 still? no -
	// host now calm: hostAvail=600, heap calm) => target drops -> streak increments.
	gcGovernor(0.50, 600, 0.1, true, &st) // full sweep: host 600 -> hostLevel 0, target 0 < level 2
	if st.consecutiveCalmCount <= before {
		t.Fatalf("expected full sweep to accumulate calm after host recovered, count=%d", st.consecutiveCalmCount)
	}
}
