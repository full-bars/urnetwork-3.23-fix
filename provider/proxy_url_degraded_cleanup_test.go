package main

import (
	"testing"
	"time"
)

// TestDegradedCleanup_UnsetThresholdUsesDefault verifies that when
// DegradedCleanupThreshold is unset or empty, the 6-hour default is used:
// members degraded longer than 6h are removed, while members degraded less
// than 6h are preserved.
func TestDegradedCleanup_UnsetThresholdUsesDefault(t *testing.T) {
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
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[oldAddr]; ok {
		t.Errorf("expected %s (degraded 7h) to be removed", oldAddr)
	}
	if _, ok := urlState.Cache[youngAddr]; !ok {
		t.Errorf("expected %s (degraded 5h) to be kept under 6h default", youngAddr)
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

// TestDegradedCleanup_EmptyDownSinceBackfill verifies that a degraded member
// without a DownSince is backfilled rather than deleted on sight on the first
// pass, and ages into removal on a pass after the window.
func TestDegradedCleanup_EmptyDownSinceBackfill(t *testing.T) {
	withTempHome(t)

	now := time.Now()

	// Subtest 1: Degraded member with no DownSince and no LastGraded.
	// First pass backfills DownSince to the read moment and does NOT remove it.
	// After the window elapses, a subsequent pass removes it.
	t.Run("backfill_from_reading_moment_not_deleted_on_first_pass", func(t *testing.T) {
		addr := "20.0.0.1:1080"
		if err := writeProxyURLState(&ProxyURLState{
			Cache: map[string]ProxyURLEntry{addr: {}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyState(&ProxyState{
			StartedAt: now.Add(-70 * time.Minute),
			Proxies: map[string]ProxyEntry{
				addr: {
					Health: "recently_offline",
					Source: "url",
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		// Verify backfill happens on read:
		st, err := readProxyState()
		if err != nil {
			t.Fatal(err)
		}
		if st.Proxies[addr].DownSince == "" {
			t.Fatalf("%s DownSince was not backfilled on read", addr)
		}
		ds, err := time.Parse(time.RFC3339, st.Proxies[addr].DownSince)
		if err != nil || time.Since(ds) > time.Minute {
			t.Fatalf("%s DownSince = %q, want recent reading moment", addr, st.Proxies[addr].DownSince)
		}

		// First pass: freshly backfilled down clock is within the 6h window -> not removed.
		removed := runProxyURLCleanupOnce("url")
		if removed != 0 {
			t.Fatalf("first pass removed = %d, want 0 (must not delete on sight)", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[addr]; !ok {
			t.Fatalf("member %s was deleted on sight instead of waiting for window", addr)
		}

		// Simulate the member aging past the 6h default window:
		st, _ = readProxyState()
		entry := st.Proxies[addr]
		entry.DownSince = now.Add(-7 * time.Hour).UTC().Format(time.RFC3339)
		st.Proxies[addr] = entry
		if err := writeProxyState(st); err != nil {
			t.Fatal(err)
		}

		// Second pass after aging: removed.
		removed2 := runProxyURLCleanupOnce("url")
		if removed2 != 1 {
			t.Fatalf("second pass after aging removed = %d, want 1", removed2)
		}
		urlState2, _ := readProxyURLState()
		if _, ok := urlState2.Cache[addr]; ok {
			t.Fatalf("aged member %s should have been removed on second pass", addr)
		}
	})

	// Subtest 2: Degraded member with no DownSince, but LastGraded is set 7h ago.
	// Backfilled from LastGraded; since LastGraded > 6h ago, it is removed on the pass.
	t.Run("backfill_from_last_graded", func(t *testing.T) {
		addr := "20.0.0.2:1080"
		if err := writeProxyURLState(&ProxyURLState{
			Cache: map[string]ProxyURLEntry{addr: {}},
		}); err != nil {
			t.Fatal(err)
		}
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

		st, err := readProxyState()
		if err != nil {
			t.Fatal(err)
		}
		if st.Proxies[addr].DownSince == "" {
			t.Fatalf("%s DownSince was not backfilled", addr)
		}
		ds, err := time.Parse(time.RFC3339, st.Proxies[addr].DownSince)
		if err != nil || time.Since(ds) < 6*time.Hour {
			t.Fatalf("%s DownSince = %q, want ~7h ago from LastGraded", addr, st.Proxies[addr].DownSince)
		}

		removed := runProxyURLCleanupOnce("url")
		if removed != 1 {
			t.Fatalf("removed = %d, want 1 for member whose LastGraded was 7h ago", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[addr]; ok {
			t.Fatalf("member %s with LastGraded 7h ago should have been removed", addr)
		}
	})
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
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{addr: {}},
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
