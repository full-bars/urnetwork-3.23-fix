package main

import (
	"sort"
	"testing"
)

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

// TestCollectRankedCandidates_RankSortedDescending pins the precondition
// the merge's continue→break relies on: the production caller
// (fetchAndMergeProxyURLs) feeds mergeProxyURLEntries rank-sorted input,
// best first. If a future caller ever feeds raw lines, admissible
// high-tier candidates could be silently dropped at cap with no test or
// assertion firing (Opus review test gap).
func TestCollectRankedCandidates_RankSortedDescending(t *testing.T) {
	fetched := [][]string{
		{"f:1080", "a:1080", "ungraded:1080", "c:1080", "b:1080", "d:1080"},
	}
	grades := map[string]proxyURLGrade{
		"a:1080": {Score: 0.95, Decidable: true},
		"b:1080": {Score: 0.85, Decidable: true},
		"c:1080": {Score: 0.75, Decidable: true},
		"d:1080": {Score: 0.65, Decidable: true},
		"f:1080": {Score: 0.3, Decidable: true},
		// ungraded:1080 intentionally absent from grades
	}
	cands := collectRankedCandidates(fetched, grades)
	if len(cands) != 6 {
		t.Fatalf("expected 6 candidates, got %d", len(cands))
	}
	if !sort.SliceIsSorted(cands, func(i, j int) bool { return cands[i].rank > cands[j].rank }) {
		t.Error("candidates must be rank-sorted descending (best first)")
	}
	if cands[0].address != "a:1080" {
		t.Errorf("first candidate %s, want a:1080", cands[0].address)
	}
	last := cands[len(cands)-1]
	if last.address != "ungraded:1080" || last.rank != -1 {
		t.Errorf("last candidate %s rank %d, want ungraded:1080 rank -1", last.address, last.rank)
	}
}

// TestMergeProxyURLEntries_MaxTotalZeroUnlimited pins that maxTotal == 0
// (unlimited) never enters the eviction block even with a non-nil rank
// function: every candidate is added and no cached entry is evicted, no
// matter how low its tier is (Opus review test gap).
func TestMergeProxyURLEntries_MaxTotalZeroUnlimited(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {Graded: true, Score: 0.3},  // F
		"2.2.2.2:1080": {Graded: true, Score: 0.95}, // A
	}}
	rank := func(addr string) int { return proxyTierRank("A") } // any candidate outranks both
	added := mergeProxyURLEntries(state, []string{"new-a:1080", "new-b:1080", "new-c:1080"}, 1, 0, rank, nil)
	if added != 3 {
		t.Fatalf("unlimited cache: expected 3 added, got %d", added)
	}
	if len(state.Cache) != 5 {
		t.Errorf("cache size %d, want 5 (no eviction in unlimited mode)", len(state.Cache))
	}
	for _, addr := range []string{"1.1.1.1:1080", "2.2.2.2:1080"} {
		if _, ok := state.Cache[addr]; !ok {
			t.Errorf("unlimited mode must not evict %s", addr)
		}
	}
}

// TestMergeProxyURLEntries_UngradedJustAddedIsNextEvictionTarget pins the
// doc-named hazard (gradeFor nil + rankAddr non-nil): a just-added entry
// with no persisted grade ranks -1, so the very next line in the same call
// can evict it — churn within a single merge with an inflated `added`
// count. The production caller always passes gradeFor (every pooled
// candidate carries a grade by construction), so this combination is
// unreachable in production; the test pins the current behavior so any
// future fix that makes the combination impossible has a regression signal
// (Opus review test gap).
func TestMergeProxyURLEntries_UngradedJustAddedIsNextEvictionTarget(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {},                          // ungraded, rank -1
		"2.2.2.2:1080": {Graded: true, Score: 0.65}, // D
	}}
	rank := func(addr string) int { return proxyTierRank("C") }
	added := mergeProxyURLEntries(state, []string{"new-c1:1080", "new-c2:1080"}, 2, 2, rank, nil)
	if added != 2 {
		t.Fatalf("expected 2 added (with same-call churn), got %d", added)
	}
	if _, ok := state.Cache["new-c2:1080"]; !ok {
		t.Error("second line should be cached")
	}
	if _, still := state.Cache["new-c1:1080"]; still {
		t.Error("just-added ungraded c1 should have been evicted by the next line (doc-named hazard)")
	}
	if _, still := state.Cache["1.1.1.1:1080"]; still {
		t.Error("ungraded 1.1.1.1 should have been evicted by the first line")
	}
	if _, ok := state.Cache["2.2.2.2:1080"]; !ok {
		t.Error("graded D entry should survive (rank 1 < candidate rank 2)")
	}
}
