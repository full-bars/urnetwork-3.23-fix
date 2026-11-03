package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestDegradedCleanup_UnsetThresholdRetiresNothing verifies that when
// DegradedCleanupThreshold is unset or empty, degraded eviction is opt-in:
// with no threshold configured, a member that has been degraded for longer
// than any plausible window is still kept. Deleting proxy capacity must
// never be the default behaviour.
func TestDegradedCleanup_UnsetThresholdRetiresNothing(t *testing.T) {
	withTempHome(t)

	oldAddr := "1.1.1.1:1080"
	youngAddr := "2.2.2.2:1080"

	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			oldAddr:   {},
			youngAddr: {},
		},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	state := &ProxyState{
		StartedAt: now.Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			oldAddr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: now.Add(-7 * time.Hour).UTC().Format(time.RFC3339),
			},
			youngAddr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: now.Add(-5 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("removed = %d with no threshold configured, want 0", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[oldAddr]; !ok {
		t.Errorf("expected %s (degraded 7h) to be KEPT when eviction is off", oldAddr)
	}
	if _, ok := urlState.Cache[youngAddr]; !ok {
		t.Errorf("expected %s to be kept when eviction is off", youngAddr)
	}
}

// TestDegradedCleanup_ExplicitThresholdWins verifies that an explicitly configured
// threshold wins over the 6-hour default, both when shorter and when longer.
func TestDegradedCleanup_ExplicitThresholdWins(t *testing.T) {
	withTempHome(t)

	now := time.Now()

	// Subtest 1: Shorter explicit threshold (1h) evicts a member degraded for 2h
	// which would have been kept by the 6h default.
	t.Run("shorter_wins", func(t *testing.T) {
		addr := "10.0.0.1:1080"
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: "1h",
			Cache: map[string]ProxyURLEntry{
				addr: {},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyState(&ProxyState{
			StartedAt: now.Add(-70 * time.Minute),
			Proxies: map[string]ProxyEntry{
				addr: {
					Health:    "offline",
					Source:    "url",
					DownSince: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		removed := runProxyURLCleanupOnce("url")
		if removed != 1 {
			t.Fatalf("removed = %d, want 1 with 1h explicit threshold", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[addr]; ok {
			t.Errorf("expected %s to be evicted with 1h threshold", addr)
		}
	})

	// Subtest 2: Longer explicit threshold (12h) preserves a member degraded for 8h
	// which would have been evicted by the 6h default, but removes one degraded for 14h.
	t.Run("longer_wins", func(t *testing.T) {
		eightHourAddr := "10.0.0.8:1080"
		fourteenHourAddr := "10.0.0.14:1080"
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: "12h",
			Cache: map[string]ProxyURLEntry{
				eightHourAddr:    {},
				fourteenHourAddr: {},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyState(&ProxyState{
			StartedAt: now.Add(-70 * time.Minute),
			Proxies: map[string]ProxyEntry{
				eightHourAddr: {
					Health:    "long_offline",
					Source:    "url",
					DownSince: now.Add(-8 * time.Hour).UTC().Format(time.RFC3339),
				},
				fourteenHourAddr: {
					Health:    "long_offline",
					Source:    "url",
					DownSince: now.Add(-14 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		removed := runProxyURLCleanupOnce("url")
		if removed != 1 {
			t.Fatalf("removed = %d, want 1 with 12h explicit threshold", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[eightHourAddr]; !ok {
			t.Errorf("expected %s (8h down) to be kept with 12h threshold", eightHourAddr)
		}
		if _, ok := urlState.Cache[fourteenHourAddr]; ok {
			t.Errorf("expected %s (14h down) to be evicted with 12h threshold", fourteenHourAddr)
		}
	})
}

// TestDegradedCleanup_HiccupNotRetired verifies that a member graded seven hours
// ago that blipped or degraded recently (e.g. 10m ago or legacy without DownSince)
// is NOT retired based on LastGraded. LastGraded reflects when a proxy was alive
// and working, not when it went down.
func TestDegradedCleanup_HiccupNotRetired(t *testing.T) {
	withTempHome(t)

	addr := "20.0.0.1:1080"
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{addr: {}},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	// Member was last graded 7h ago while alive and working, and has empty DownSince on disk (legacy).
	// It must NOT be backfilled from LastGraded and instantly deleted.
	if err := writeProxyState(&ProxyState{
		StartedAt: now.Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:     "recently_offline",
				Source:     "url",
				LastGraded: now.Add(-7 * time.Hour),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("hiccup member was retired: removed = %d, want 0", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("hiccup member %s was evicted from cache", addr)
	}
}

// TestDegradedCleanup_LegacyEmptyDownSincePersisted verifies that a legacy member
// with an empty DownSince gets stamped from the read moment, the stamp is persisted
// to disk immediately, and a subsequent read sees the same persisted timestamp
// rather than generating a fresh one.
func TestDegradedCleanup_LegacyEmptyDownSincePersisted(t *testing.T) {
	withTempHome(t)

	addr := "20.0.0.2:1080"
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			addr: {
				Health: "recently_offline",
				Source: "url",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// First read stamps DownSince and persists it to disk.
	firstState, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	firstStamp := firstState.Proxies[addr].DownSince
	if firstStamp == "" {
		t.Fatalf("%s DownSince was not stamped on first read", addr)
	}

	time.Sleep(15 * time.Millisecond)

	// Second read must see the identical persisted timestamp, not a fresh reading moment.
	secondState, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	secondStamp := secondState.Proxies[addr].DownSince
	if secondStamp != firstStamp {
		t.Fatalf("second read saw fresh stamp %q, want persisted %q", secondStamp, firstStamp)
	}

	// Verify the stamp is physically persisted in the disk file.
	path, err := proxyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), firstStamp) {
		t.Fatalf("persisted proxy.state does not contain timestamp %q:\n%s", firstStamp, string(raw))
	}
}

// TestDegradedCleanup_TrimHeldNotRetired verifies that a degraded member held
// back by the trim cap is not retired, even if degraded past the threshold.
func TestDegradedCleanup_TrimHeldNotRetired(t *testing.T) {
	withTempHome(t)

	upAddr := "20.0.0.10:1080"
	degradedAddr := "20.0.0.11:1080"

	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			upAddr:       {},
			degradedAddr: {},
		},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := writeProxyState(&ProxyState{
		StartedAt: now.Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			upAddr: {
				Health: "up",
				Source: "url",
			},
			degradedAddr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: now.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Set trim cap to 1. Total proxies = 2.
	// degradedAddr has worse health rank than upAddr, so degradedAddr is held.
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("trim-held degraded member was retired: removed = %d, want 0", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[degradedAddr]; !ok {
		t.Fatalf("trim-held degraded member %s was evicted from cache", degradedAddr)
	}
}

// TestDegradedCleanup_UnparseableThresholdWarns verifies that when degraded_cleanup_threshold
// cannot be parsed or is negative/non-positive, a warning naming the value and fallback window
// is logged, and degraded eviction stays off.
func TestDegradedCleanup_UnparseableThresholdWarns(t *testing.T) {
	withTempHome(t)

	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: bad,
		}); err != nil {
			t.Fatal(err)
		}

		out := captureTlog(t, func() {
			runProxyURLCleanupOnce("url")
		})
		if !strings.Contains(out, bad) {
			t.Fatalf("expected warning for bad threshold %q, got:\n%s", bad, out)
		}
		if !strings.Contains(out, "leaving degraded eviction off") {
			t.Fatalf("expected warning to say eviction stays off, got:\n%s", out)
		}
	}
}

// TestDegradedCleanup_FreshlyDegradedUntouched verifies that a degraded member
// whose DownSince is well inside the window is untouched.
func TestDegradedCleanup_FreshlyDegradedUntouched(t *testing.T) {
	withTempHome(t)

	addr := "30.0.0.1:1080"
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{addr: {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("freshly degraded member removed: %d, want 0", removed)
	}
	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("freshly degraded member %s was unexpectedly removed", addr)
	}
}

// TestDegradedCleanup_InternalSourceNeverRemoved verifies that internal-source
// members in degraded state are never removed by URL cleanup, even when degraded
// for days.
func TestDegradedCleanup_InternalSourceNeverRemoved(t *testing.T) {
	withTempHome(t)

	internalAddr := "40.0.0.1:1080"
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			internalAddr: {
				Health:    "recently_offline",
				Source:    "internal",
				DownSince: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("internal-source degraded member removed: %d, want 0", removed)
	}
	st, _ := readProxyState()
	if _, ok := st.Proxies[internalAddr]; !ok {
		t.Fatalf("internal member %s was removed from proxy.state", internalAddr)
	}
}

// TestDegradedCleanup_UptimeGuardHolds verifies that the 65-minute uptime guard
// prevents removal of degraded members during provider warmup.
func TestDegradedCleanup_UptimeGuardHolds(t *testing.T) {
	withTempHome(t)

	addr := "50.0.0.1:1080"
	// A window is configured on purpose: without one, eviction is off and this
	// test would pass without ever exercising the warmup guard it exists for.
	if err := writeProxyURLState(&ProxyURLState{
		Cache:                    map[string]ProxyURLEntry{addr: {}},
		DegradedCleanupThreshold: "1h",
	}); err != nil {
		t.Fatal(err)
	}
	// Warmup: StartedAt is 10 minutes ago (< 65m minUptime).
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-10 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("warmup cleanup removed %d proxies, want 0", removed)
	}
	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("degraded member %s was evicted during warmup", addr)
	}

	// Settled: StartedAt is 70 minutes ago (> 65m minUptime).
	st, _ := readProxyState()
	st.StartedAt = time.Now().Add(-70 * time.Minute)
	if err := writeProxyState(st); err != nil {
		t.Fatal(err)
	}

	removedSettled := runProxyURLCleanupOnce("url")
	if removedSettled != 1 {
		t.Fatalf("settled cleanup removed %d proxies, want 1", removedSettled)
	}
	urlState2, _ := readProxyURLState()
	if _, ok := urlState2.Cache[addr]; ok {
		t.Fatalf("degraded member %s was not evicted after warmup elapsed", addr)
	}
}
