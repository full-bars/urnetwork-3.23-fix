package urnettools

import (
	"errors"
	"testing"
)

// A hotswap starts the new provider while the old one is still draining, so both
// are resident at once. On a box already near its memory limit that doubles
// usage and gets a process OOM-killed. The preflight must decline in that case,
// which makes the update fall back to a plain service restart (one process at a
// time), exactly as it already does for an old version or a Type=simple unit.
func TestHotSwapMemoryOK(t *testing.T) {
	const mib = int64(1) << 20
	cases := []struct {
		name         string
		avail, rss   int64
		wantDeclined bool
	}{
		{"plenty of room", 2000 * mib, 800 * mib, false},
		{"exactly 1.1x the provider's RSS", 880 * mib, 800 * mib, false},
		{"just under 1.1x", 879 * mib, 800 * mib, true},
		{"the incident box: 600 MB free, 850 MB provider", 600 * mib, 850 * mib, true},
		{"unknown RSS never blocks", 100 * mib, 0, false},
		{"unknown availability never blocks", -1, 800 * mib, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := hotSwapMemoryOK(c.avail, c.rss)
			if c.wantDeclined != (err != nil) {
				t.Fatalf("hotSwapMemoryOK(%d, %d) = %v, declined=%v want %v", c.avail, c.rss, err, err != nil, c.wantDeclined)
			}
			if err != nil && !errors.Is(err, ErrHotSwapLowMemory) {
				t.Fatalf("decline must wrap ErrHotSwapLowMemory, got %v", err)
			}
		})
	}
}

func TestParseKiBField(t *testing.T) {
	meminfo := "MemTotal:        1977320 kB\nMemFree:           70000 kB\nMemAvailable:     707124 kB\nCached:           735488 kB\n"
	if v, ok := parseKiBField(meminfo, "MemAvailable"); !ok || v != 707124 {
		t.Fatalf("MemAvailable = %d ok=%v", v, ok)
	}
	if _, ok := parseKiBField(meminfo, "Mem"); ok {
		t.Fatalf("a key prefix must not match a longer field name")
	}
	if _, ok := parseKiBField(meminfo, "SwapFree"); ok {
		t.Fatalf("a missing field must report not found")
	}
	status := "Name:\turnetwork\nVmRSS:\t  435544 kB\nThreads:\t11\n"
	if v, ok := parseKiBField(status, "VmRSS"); !ok || v != 435544 {
		t.Fatalf("VmRSS = %d ok=%v", v, ok)
	}
	if _, ok := parseKiBField("VmRSS:\tlots kB\n", "VmRSS"); ok {
		t.Fatalf("a non-numeric value must report not found")
	}
}

func TestHotSwapPreflightDeclinesWhenBothProvidersWouldNotFit(t *testing.T) {
	const mib = int64(1) << 20
	origUnit, origMem := unitTypeFunc, hotSwapMemoryFunc
	t.Cleanup(func() { unitTypeFunc, hotSwapMemoryFunc = origUnit, origMem })
	unitTypeFunc = func(Provider) (string, error) { return "notify", nil }
	p := Provider{PID: 424242, Version: "v3.23.0-fix.32.7", Unit: "urnetwork.service"}

	hotSwapMemoryFunc = func(pid int) (int64, int64, bool) {
		if pid != 424242 {
			t.Fatalf("memory read for pid %d, want the provider's 424242", pid)
		}
		return 600 * mib, 850 * mib, true
	}
	err := hotSwapPreflight(p)
	if !errors.Is(err, ErrHotSwapLowMemory) {
		t.Fatalf("hotSwapPreflight = %v, want ErrHotSwapLowMemory", err)
	}
	if got := hotswapDeclineReason(err); got != "low_memory" {
		t.Fatalf("decline metric label = %q, want low_memory", got)
	}

	hotSwapMemoryFunc = func(int) (int64, int64, bool) { return 2000 * mib, 850 * mib, true }
	if err := hotSwapPreflight(p); err != nil {
		t.Fatalf("with room to spare hotSwapPreflight = %v, want nil", err)
	}

	// Memory could not be read (non-Linux, /proc hidden): keep the old behavior
	// rather than losing zero-downtime on a guess.
	hotSwapMemoryFunc = func(int) (int64, int64, bool) { return 0, 0, false }
	if err := hotSwapPreflight(p); err != nil {
		t.Fatalf("unreadable memory must not block a hotswap, got %v", err)
	}

	// No PID (not running / Docker path): nothing to measure, never blocks.
	hotSwapMemoryFunc = func(int) (int64, int64, bool) {
		t.Fatalf("memory must not be read without a PID")
		return 0, 0, false
	}
	if err := hotSwapPreflight(Provider{Version: "v3.23.0-fix.32.7", Unit: "urnetwork.service"}); err != nil {
		t.Fatalf("no PID: hotSwapPreflight = %v, want nil", err)
	}
}
