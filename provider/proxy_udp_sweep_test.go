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

// An identity whose IPv6 retry is due is scheduled ahead of candidates that are
// only due for their udp recheck, inside the same batch budget: its retry must
// run on its own interval, not wait most of a twelve hour cycle.
func TestSelectUDPProbeBatchPutsIpv6DueFirst(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	candidates := []udpProbeCandidate{
		// recently measured, but its ipv6 retry is due now
		{key: "ipv6-due", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute), ipv6Due: true},
		{key: "old-a", online: true, status: proxyUDPOK, lastChecked: now.Add(-13 * time.Hour)},
		{key: "old-b", online: true, status: proxyUDPOK, lastChecked: now.Add(-14 * time.Hour)},
		{key: "old-c", online: true, status: proxyUDPOK, lastChecked: now.Add(-15 * time.Hour)},
	}
	// 4 online, a one minute tick and a twelve hour effective window select one
	batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, time.Minute, nil)
	if len(batch) != 1 {
		t.Fatalf("batch size = %d, want the unchanged budget of 1", len(batch))
	}
	if batch[0].key != "ipv6-due" {
		t.Fatalf("first candidate = %q, want the ipv6-due one", batch[0].key)
	}

	// with room for two, a second ipv6-due candidate follows
	candidates = append(candidates, udpProbeCandidate{key: "ipv6-due-2", online: true, status: proxyUDPOK, lastChecked: now.Add(-2 * time.Minute), ipv6Due: true})
	// five online and a three hour tick over the twelve hour effective window
	// select two
	batch = selectUDPProbeBatch(now, candidates, 24*time.Hour, 3*time.Hour, nil)
	if len(batch) != 2 {
		t.Fatalf("batch size = %d, want 2", len(batch))
	}
	if batch[0].key != "ipv6-due-2" && batch[0].key != "ipv6-due" {
		t.Fatalf("first candidate = %q, want an ipv6-due one", batch[0].key)
	}
	if batch[1].key != "ipv6-due-2" && batch[1].key != "ipv6-due" {
		t.Fatalf("second candidate = %q, want the other ipv6-due one", batch[1].key)
	}
}
