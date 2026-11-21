package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func TestSelectUDPProbeDueBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		status proxyUDPStatus
		ttl    time.Duration
	}{
		{"ok", proxyUDPOK, 24 * time.Hour},
		{"no-relay", proxyUDPNoRelay, 72 * time.Hour},
		{"mismatch", proxyUDPExitMismatch, 24 * time.Hour},
		{"unknown", proxyUDPUnknown, time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidates := []udpProbeCandidate{
				{key: "before", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl/2 + time.Nanosecond)},
				{key: "at", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl / 2)},
				{key: "after", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl/2 - time.Nanosecond)},
				{key: "offline", status: tt.status},
			}
			got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, rand.New(rand.NewSource(1)))
			if len(got) != 2 || got[0].key != "after" || got[1].key != "at" {
				t.Fatalf("due candidates = %v, want after, at", got)
			}
		})
	}
}

func TestSelectUDPProbeBatchSizes(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ online, want int }{{1, 1}, {7, 1}, {4750, 4}} {
		t.Run(fmt.Sprint(tt.online), func(t *testing.T) {
			candidates := make([]udpProbeCandidate, tt.online)
			for i := range candidates {
				candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true}
			}
			got := selectUDPProbeBatch(now, candidates, 24*time.Hour, time.Minute, rand.New(rand.NewSource(42)))
			if len(got) != tt.want {
				t.Fatalf("batch size = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestSelectUDPProbeJitter(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candidates := make([]udpProbeCandidate, 150)
	for i := range candidates {
		candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true}
	}
	seenLow, seenHigh := false, false
	for seed := int64(0); seed < 1000; seed++ {
		got := selectUDPProbeBatch(now, candidates, 3*time.Minute, time.Minute, rand.New(rand.NewSource(seed)))
		if len(got) < 40 || len(got) > 60 {
			t.Fatalf("seed %d: size %d outside [40,60]", seed, len(got))
		}
		seenLow = seenLow || len(got) < 50
		seenHigh = seenHigh || len(got) > 50
	}
	if !seenLow || !seenHigh {
		t.Fatal("jitter did not vary on both sides of the base size")
	}
	// A tick longer than the window still uses the uncapped formula before
	// jitter; 100 * 11/10 gives a base of 110, with a lower bound of 88.
	seenBelowCap := false
	for seed := int64(0); seed < 1000; seed++ {
		got := selectUDPProbeBatch(now, candidates[:100], 10*time.Minute, 11*time.Minute, rand.New(rand.NewSource(seed)))
		if len(got) < 88 || len(got) > 100 {
			t.Fatalf("long tick seed %d: size %d outside [88,100]", seed, len(got))
		}
		seenBelowCap = seenBelowCap || len(got) < 100
	}
	if !seenBelowCap {
		t.Fatal("long tick jitter never dropped below the due cap")
	}
}

func TestSelectUDPProbeOrdering(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candidates := []udpProbeCandidate{
		{key: "tie-b", online: true, lastChecked: now.Add(-48 * time.Hour)},
		{key: "new-b", online: true},
		{key: "oldest", online: true, lastChecked: now.Add(-72 * time.Hour)},
		{key: "tie-a", online: true, lastChecked: now.Add(-48 * time.Hour)},
		{key: "new-a", online: true},
	}
	original := append([]udpProbeCandidate(nil), candidates...)
	got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, nil)
	var keys []string
	for _, candidate := range got {
		keys = append(keys, candidate.key)
	}
	want := []string{"new-a", "new-b", "oldest", "tie-a", "tie-b"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("order = %v, want %v", keys, want)
	}
	if !reflect.DeepEqual(candidates, original) {
		t.Fatal("selection modified the input")
	}
}

func TestSelectUDPProbeEmptyAndLimits(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	fresh := []udpProbeCandidate{{key: "fresh", online: true, status: proxyUDPOK, lastChecked: now}}
	for _, tt := range []struct {
		name         string
		candidates   []udpProbeCandidate
		window, tick time.Duration
	}{
		{"empty", nil, time.Hour, time.Minute},
		{"not due", fresh, time.Hour, time.Minute},
		{"zero window", []udpProbeCandidate{{online: true}}, 0, time.Minute},
		{"zero tick", []udpProbeCandidate{{online: true}}, time.Hour, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectUDPProbeBatch(now, tt.candidates, tt.window, tt.tick, rand.New(rand.NewSource(1))); len(got) != 0 {
				t.Fatalf("got %v, want no candidates", got)
			}
		})
	}
	candidates := make([]udpProbeCandidate, 100)
	for i := range candidates {
		candidates[i] = udpProbeCandidate{online: true, status: proxyUDPOK, lastChecked: now}
	}
	candidates[0].lastChecked = time.Time{}
	if got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, rand.New(rand.NewSource(2))); len(got) != 1 {
		t.Fatalf("due cap = %d, want 1", len(got))
	}
}

// The selector orders by the earlier due time, so neither kind of due time has a
// standing priority: the oldest outstanding due goes first.
func TestSelectUDPProbeBatchOrdersByTheEarliestDueTime(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	ipv6Earliest := []udpProbeCandidate{
		{key: "ipv6-earlier", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute), ipv6Due: true, ipv6DueAt: now.Add(-30 * time.Minute)},
		{key: "udp-later", online: true, status: proxyUDPOK, lastChecked: now.Add(-11 * time.Hour)},
	}
	batch := selectUDPProbeBatch(now, ipv6Earliest, 24*time.Hour, time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "ipv6-earlier" {
		t.Fatalf("batch = %v, want the identity whose ipv6 retry is the oldest due", batch)
	}

	udpEarliest := []udpProbeCandidate{
		{key: "udp-earlier", online: true, status: proxyUDPOK, lastChecked: now.Add(-20 * time.Hour)},
		{key: "ipv6-later", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute), ipv6Due: true, ipv6DueAt: now.Add(-10 * time.Minute)},
	}
	batch = selectUDPProbeBatch(now, udpEarliest, 24*time.Hour, time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "udp-earlier" {
		t.Fatalf("batch = %v, want the identity whose udp recheck is the oldest due", batch)
	}
}

// Sol's trace: four identities hold the batch with IPv6 retries that are only
// due soon, and one identity's udp evidence expired hours ago. The expired one
// is the earliest due, so it takes the single slot.
func TestSelectUDPProbeBatchSelectsTheOldestDueOverIpv6Retries(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var candidates []udpProbeCandidate
	for i := 0; i < 4; i++ {
		candidates = append(candidates, udpProbeCandidate{
			key:         fmt.Sprintf("retry-%d", i),
			online:      true,
			status:      proxyUDPNoRelay,
			lastChecked: now.Add(-time.Minute),
			ipv6Due:     true,
			// the stage stopped fifteen minutes before this tick
			ipv6DueAt: now.Add(15 * time.Minute),
		})
	}
	candidates = append(candidates, udpProbeCandidate{
		key:         "expired-udp",
		online:      true,
		status:      proxyUDPOK,
		lastChecked: now.Add(-25 * time.Hour),
	})
	batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
	if len(batch) != 1 {
		t.Fatalf("batch size = %d, want the budget of 1", len(batch))
	}
	if batch[0].key != "expired-udp" {
		t.Fatalf("first candidate = %q, want the identity whose udp evidence expired first", batch[0].key)
	}
}

// Equal due times fall back to the older check, then the key.
func TestSelectUDPProbeBatchBreaksDueTimeTies(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	same := now.Add(-20 * time.Hour)
	candidates := []udpProbeCandidate{
		{key: "b", online: true, status: proxyUDPOK, lastChecked: same},
		{key: "a", online: true, status: proxyUDPOK, lastChecked: same},
		{key: "older", online: true, status: proxyUDPOK, lastChecked: now.Add(-21 * time.Hour)},
	}
	batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "older" {
		t.Fatalf("batch = %v, want the older check first", batch)
	}
	two := selectUDPProbeBatch(now, candidates, 24*time.Hour, 12*time.Hour, nil)
	if len(two) < 2 || two[1].key != "a" {
		t.Fatalf("batch = %v, want the key order to break a due time tie", two)
	}
}

// An identity that was never checked must get a measurement even while IPv6
// retries are due. IPv6-due candidates sorted first, so with a small budget they
// can fill every batch and an unchecked identity never runs.
func TestSelectUDPProbeBatchDoesNotStarveAnUncheckedIdentity(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var candidates []udpProbeCandidate
	for i := 0; i < 20; i++ {
		// measured a minute ago, so only the IPv6 retry makes it due, and it
		// stays un-held: this is the state a stage that ran and learned nothing
		// leaves behind
		candidates = append(candidates, udpProbeCandidate{
			key:         fmt.Sprintf("v6-%02d", i),
			online:      true,
			status:      proxyUDPOK,
			lastChecked: now.Add(-time.Minute),
			ipv6Due:     true,
		})
	}
	candidates = append(candidates, udpProbeCandidate{key: "unchecked", online: true, status: proxyUDPUnknown})

	seen := map[string]bool{}
	for tick := 0; tick < 10; tick++ {
		at := now.Add(time.Duration(tick) * time.Minute)
		batch := selectUDPProbeBatch(at, candidates, 24*time.Hour, time.Minute, nil)
		if len(batch) != 1 {
			t.Fatalf("tick %d selected %d candidates, want the budget of 1", tick, len(batch))
		}
		for _, candidate := range batch {
			seen[candidate.key] = true
		}
	}
	if !seen["unchecked"] {
		t.Fatal("an unchecked identity never reached a batch while the ipv6 retries were due")
	}
}
