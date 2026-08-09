package main

import "testing"

// TestMergeProxyURLEntries_BestOverallEviction pins the cap behavior: when
// the cache is full, a new higher-tier proxy must evict the lowest-tier
// cached entry (best-overall selection across sources), not be dropped just
// because it arrived later.
func TestMergeProxyURLEntries_BestOverallEviction(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		// lowest tier first: an ungraded F entry and a graded D
		"1.1.1.1:1080": {Graded: true, Score: 0.55}, // F
		"2.2.2.2:1080": {Graded: true, Score: 0.65}, // D
		"3.3.3.3:1080": {Graded: true, Score: 0.75}, // C
		"4.4.4.4:1080": {Graded: true, Score: 0.85}, // B
	}}

	// rankAddr only needs to rank CANDIDATES; cached entries are ranked from
	// their persisted grade by rankCacheEntry internally.
	rank := func(addr string) int {
		switch addr {
		case "new-b:1080":
			return proxyTierRank("B")
		case "new-a:1080":
			return proxyTierRank("A")
		case "new-f:1080":
			return proxyTierRank("F")
		default:
			return -1
		}
	}
	// gradeFor gives newly-added candidates their persisted grade, so a
	// just-added B entry ranks B (not ungraded -1) against later candidates.
	grade := func(addr string) (proxyURLGrade, bool) {
		switch addr {
		case "new-b:1080":
			return proxyURLGrade{Score: 0.82, Decidable: true}, true
		case "new-a:1080":
			return proxyURLGrade{Score: 0.95, Decidable: true}, true
		case "new-f:1080":
			return proxyURLGrade{Score: 0.3, Decidable: true}, true
		default:
			return proxyURLGrade{}, false
		}
	}

	// New B-tier proxy with cache full: must evict the F entry and be added.
	added := mergeProxyURLEntries(state, []string{"new-b:1080"}, 1, 4, rank, grade)
	if added != 1 {
		t.Fatalf("B-tier proxy with full cache: expected 1 added, got %d", added)
	}
	if _, ok := state.Cache["new-b:1080"]; !ok {
		t.Fatal("B-tier proxy should have been added")
	}
	if _, still := state.Cache["1.1.1.1:1080"]; still {
		t.Error("lowest-tier (F) entry should have been evicted to make room")
	}

	// New A-tier proxy: evicts the next-lowest (D).
	added = mergeProxyURLEntries(state, []string{"new-a:1080"}, 1, 4, rank, grade)
	if added != 1 {
		t.Fatalf("A-tier proxy: expected 1 added, got %d", added)
	}
	if _, ok := state.Cache["new-a:1080"]; !ok {
		t.Fatal("A-tier proxy should have been added")
	}
	if _, still := state.Cache["2.2.2.2:1080"]; still {
		t.Error("D-tier entry should have been evicted for the A-tier proxy")
	}

	// New F-tier proxy with full cache: lower than the lowest cached (C now),
	// must NOT be added, and must not evict anything.
	before := len(state.Cache)
	added = mergeProxyURLEntries(state, []string{"new-f:1080"}, 1, 4, rank, grade)
	if added != 0 {
		t.Fatalf("F-tier proxy should not be added when cache is full of better, got %d added", added)
	}
	if len(state.Cache) != before {
		t.Errorf("cache size changed %d -> %d; F-tier must not evict better entries", before, len(state.Cache))
	}
}

// TestMergeProxyURLEntries_NoRankKeepsOldBehavior pins that callers without
// a rank function (nil) keep the old first-come behavior — at cap, skip.
func TestMergeProxyURLEntries_NoRankKeepsOldBehavior(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {Graded: true, Score: 0.5},
	}}
	added := mergeProxyURLEntries(state, []string{"2.2.2.2:1080"}, 1, 1, nil, nil)
	if added != 0 {
		t.Fatalf("nil rank with full cache: expected 0 added, got %d", added)
	}
	if _, ok := state.Cache["2.2.2.2:1080"]; ok {
		t.Error("entry should not have been added at cap without a rank function")
	}
	if len(state.Cache) != 1 {
		t.Errorf("cache size %d, want 1", len(state.Cache))
	}
}

// TestMergeProxyURLEntries_EvictionPrefersUngraded pins that ungraded cache
// entries (rank -1) are the first eviction targets — never-graded entries
// are less valuable than any graded one.
func TestMergeProxyURLEntries_EvictionPrefersUngraded(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {},                          // ungraded, rank -1
		"2.2.2.2:1080": {Graded: true, Score: 0.65}, // D
	}}
	rank := func(addr string) int {
		if addr == "new-c:1080" {
			return proxyTierRank("C")
		}
		return -1
	}
	added := mergeProxyURLEntries(state, []string{"new-c:1080"}, 1, 2, rank, nil)
	if added != 1 {
		t.Fatalf("expected 1 added (evicting ungraded), got %d", added)
	}
	if _, still := state.Cache["1.1.1.1:1080"]; still {
		t.Error("ungraded entry should have been evicted first")
	}
	if _, ok := state.Cache["new-c:1080"]; !ok {
		t.Error("C-tier proxy should have been added")
	}
}

// TestMergeProxyURLEntries_CachedATierSurvives pins the rankCacheEntry
// distinction: a high-tier entry cached in a PREVIOUS cycle is not in this
// cycle's grades map, but its persisted grade must still protect it from
// eviction by lower-tier candidates. (Regression: ranking cached entries
// through the candidate closure collapsed all old entries to -1.)
func TestMergeProxyURLEntries_CachedATierSurvives(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"old-a:1080": {Graded: true, Score: 0.95}, // A, cached last cycle
		"old-f:1080": {Graded: true, Score: 0.3},  // F
	}}
	// This cycle's grades map knows only the new candidate — old-a is NOT
	// in it, exactly like a real cross-cycle fetch.
	rank := func(addr string) int {
		if addr == "new-b:1080" {
			return proxyTierRank("B")
		}
		return -1
	}
	added := mergeProxyURLEntries(state, []string{"new-b:1080"}, 1, 2, rank, nil)
	if added != 1 {
		t.Fatalf("expected 1 added, got %d", added)
	}
	if _, ok := state.Cache["old-a:1080"]; !ok {
		t.Fatal("cached A-tier entry must survive eviction by a B-tier candidate")
	}
	if _, still := state.Cache["old-f:1080"]; still {
		t.Error("cached F-tier entry should have been evicted for the B-tier candidate")
	}
}
