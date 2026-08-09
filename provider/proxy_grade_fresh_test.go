package main

import (
	"testing"
)

// TestCachedProxyAddresses pins that the fetch cycle's new-only filter
// treats every cached address as "already probed, skip" regardless of
// grade age — a cached proxy is not re-probed by the fetch cycle at all.
// Quality refresh is the reaper's job.
func TestCachedProxyAddresses(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"a:1080":        {Graded: true, Score: 0.95},
		"old:1080":      {Graded: true, Score: 0.7}, // old grade, still skipped
		"ungraded:1080": {},                         // never graded, still skipped
	}}
	addrs := cachedProxyAddresses(state)
	if len(addrs) != 3 {
		t.Fatalf("expected 3 cached addresses, got %d", len(addrs))
	}
	for _, want := range []string{"a:1080", "old:1080", "ungraded:1080"} {
		if !addrs[want] {
			t.Errorf("missing %s in cached set", want)
		}
	}

	// nil state -> empty set (probe everything).
	if n := len(cachedProxyAddresses(nil)); n != 0 {
		t.Errorf("nil state must yield empty cached set, got %d", n)
	}
}

// TestMustReadProxyURLState_NotExist pins that a missing cache file reads as
// an empty state rather than an error — the fetch cycle probes everything
// on a fresh install.
func TestMustReadProxyURLState_NotExist(t *testing.T) {
	withTempHome(t)
	state := mustReadProxyURLState()
	if state == nil || len(state.Cache) != 0 {
		t.Fatalf("expected empty state on missing file, got %+v", state)
	}
}

// TestMustReadProxyURLState_ReadsExisting pins round-trip: a written cache
// comes back intact.
func TestMustReadProxyURLState_ReadsExisting(t *testing.T) {
	withTempHome(t)
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {Graded: true, Score: 0.8},
	}}); err != nil {
		t.Fatal(err)
	}
	state := mustReadProxyURLState()
	if len(state.Cache) != 1 || !state.Cache["9.9.9.9:1080"].Graded {
		t.Fatalf("expected cached entry to round-trip, got %+v", state.Cache)
	}
}
