package main

import (
	"math"
	"testing"

	"github.com/urnetwork/connect"
)

// The goroutine sensor exists to catch a runaway (a leak that grows without
// bound), not to count proxies. A healthy proxy costs 20-27 goroutines, so a
// FIXED 25k ceiling pinned the score at 1.00 on any healthy pool above roughly
// 1,000 proxies; with the pressure score feeding the URL pool controller, the
// per-connection memory budget and the status word, a healthy big box looked
// permanently exhausted. The sensor must judge goroutines PER RUNNING PROXY.
func TestGoroutineSensorJudgesPerProxyNotPoolSize(t *testing.T) {
	healthy := []struct {
		name       string
		goroutines int
		proxies    int
	}{
		{"2 GiB box, fresh process at its 2000-proxy cap", 40771, 2001},
		{"2 GiB box before the trim", 85361, 4128},
		{"24 GB box", 105006, 4353},
		{"a 1300-proxy small box", 27525, 1303},
		// Real counts, not ideals: a 1-proxy node runs ~1,000 goroutines of
		// process overhead plus the proxy's ~20-27, and a direct-only node
		// (direct excluded from the count) sits at roughly the fixed 1,000.
		{"one proxy at the realistic overhead baseline", 1030, 1},
		{"direct-only node at the background baseline", 1000, 0},
		{"direct-only node slightly above the baseline", 1800, 0},
		{"idle: no proxies, few goroutines", 900, 0},
	}
	for _, c := range healthy {
		t.Run(c.name, func(t *testing.T) {
			score, comps := computePressure(pressureSample{Goroutines: c.goroutines, RunningProxies: c.proxies})
			if comps["goro"] != 0 || score != 0 {
				t.Fatalf("%d goroutines over %d proxies is healthy: goro=%v score=%v, want 0/0", c.goroutines, c.proxies, comps["goro"], score)
			}
		})
	}
}

func TestGoroutineSensorCatchesAPerProxyRunaway(t *testing.T) {
	const proxies = 1000
	at := func(perProxy int) (float64, float64) {
		score, comps := computePressure(pressureSample{Goroutines: goroutineFixedOverhead + perProxy*proxies, RunningProxies: proxies})
		return comps["goro"], score
	}

	if g, _ := at(int(goroutinePerProxyRampLo)); g != 0 {
		t.Fatalf("at the ramp floor the component must still be 0, got %v", g)
	}
	// Midway up the ramp: (105-60)/(150-60) = 0.5.
	mid := int((goroutinePerProxyRampLo + goroutinePerProxyRampHi) / 2)
	if g, _ := at(mid); math.Abs(g-0.5) > 0.01 {
		t.Fatalf("midway component = %v, want 0.5", g)
	}
	if g, s := at(int(goroutinePerProxyRampHi)); g != 1 || s != 1 {
		t.Fatalf("at the ramp ceiling goro=%v score=%v, want 1/1", g, s)
	}
	// Emergency pin: a runaway bypasses smoothing.
	if _, s := at(int(emergencyGoroutinesPerProxy)); s != 1 {
		t.Fatalf("emergency runaway must pin the score to 1, got %v", s)
	}
}

// With no running proxies (a direct-only node) there is nothing to divide by,
// so the original absolute ramp still applies.
func TestGoroutineSensorFallsBackToAbsoluteRampWithoutProxies(t *testing.T) {
	if _, comps := computePressure(pressureSample{Goroutines: 4000}); comps["goro"] != 0 {
		t.Fatalf("below the absolute floor: %v", comps["goro"])
	}
	if _, comps := computePressure(pressureSample{Goroutines: 15000}); math.Abs(comps["goro"]-0.5) > 0.01 {
		t.Fatalf("midway on the absolute ramp: %v, want 0.5", comps["goro"])
	}
	if s, _ := computePressure(pressureSample{Goroutines: emergencyGoroutines}); s != 1 {
		t.Fatalf("absolute emergency must pin, got %v", s)
	}
}

// The fixed process overhead (control socket, monitors, metrics, GC workers)
// must not be charged to the proxies: a small pool on a busy process is fine.
func TestGoroutineSensorDoesNotChargeProcessOverheadToASmallPool(t *testing.T) {
	score, comps := computePressure(pressureSample{Goroutines: goroutineFixedOverhead - 1, RunningProxies: 3})
	if comps["goro"] != 0 || score != 0 {
		t.Fatalf("overhead-only process: goro=%v score=%v, want 0/0", comps["goro"], score)
	}
}

// The sensor's pool count must exclude the native direct transport: it is one
// fixed goroutine, not a pool member, and counting it made a direct-only node
// divide by 1 and judge its ~1,000-goroutine background as an emergency.
func TestRunningProxyCountForPressureExcludesDirect(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)

	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("empty registry: got %d, want 0", got)
	}
	// The direct transport is registered at index 0 under the key "direct"
	// exactly as provider startup does.
	connect.RegisterProxy(0, "direct", "direct")
	connect.RegisterProxy(1, "10.0.0.1:1080", "10.0.0.1:1080")
	connect.RegisterProxy(2, "10.0.0.2:1080", "10.0.0.2:1080")
	if got := runningProxyCountForPressure(); got != 2 {
		t.Fatalf("direct + 2 proxies: got %d, want 2 (direct excluded)", got)
	}
	// A direct-only node must read ZERO running proxies so the sensor falls
	// back to the absolute ramp instead of dividing by one.
	connect.UnregisterProxy(1)
	connect.UnregisterProxy(2)
	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("direct-only node: got %d, want 0 (else per-proxy division by 1)", got)
	}
	connect.UnregisterProxy(0)
	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("nothing registered: got %d, want 0", got)
	}
}
