package main

import (
	"testing"
)

// TestRankedProxyCandidates_SortsByTierDesc pins that candidates across all
// sources are ordered best-first: A before B before C before D before F,
// with ungraded/socks5-only last. This is the ordering the merge uses so
// the cache fills with the highest-tier proxies first (the A→B→C→D funnel).
func TestRankedProxyCandidates_SortsByTierDesc(t *testing.T) {
	grades := map[string]proxyURLGrade{
		"f:1080":        {Score: 0.3, Decidable: true},
		"d:1080":        {Score: 0.65, Decidable: true},
		"a2:1080":       {Score: 0.95, Decidable: true},
		"b:1080":        {Score: 0.82, Decidable: true},
		"c:1080":        {Score: 0.72, Decidable: true},
		"a1:1080":       {Score: 0.99, Decidable: true},
		"ungraded:1080": {},                 // never graded
		"socks5:1080":   {Socks5Only: true}, // stage-0 fail
	}

	// Simulate two sources feeding one pool.
	fetched := [][]string{
		{"a2:1080", "f:1080", "b:1080"},                                 // source 1
		{"ungraded:1080", "a1:1080", "d:1080", "c:1080", "socks5:1080"}, // source 2
	}

	cands := collectRankedCandidates(fetched, grades)
	got := make([]string, 0, len(cands))
	for _, c := range cands {
		got = append(got, c.address)
	}

	wantOrder := []string{"a2:1080", "a1:1080", "b:1080", "c:1080", "d:1080", "f:1080"}
	// ungraded + socks5-only both rank -1; order between them is unspecified
	// but must come after all graded entries.
	if len(got) != 8 {
		t.Fatalf("expected 8 candidates, got %d: %v", len(got), got)
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Errorf("position %d: got %q, want %q (full order %v)", i, got[i], want, got)
		}
	}
	for _, last := range got[6:] {
		if last != "ungraded:1080" && last != "socks5:1080" {
			t.Errorf("ungraded/socks5-only must be last, got %q in tail %v", last, got[6:])
		}
	}
}

// TestRankedProxyCandidates_StableWithinTier pins that candidates with the
// same tier keep source order — the sort is stable so a higher-tier proxy
// is never displaced by a same-tier one that arrived later.
func TestRankedProxyCandidates_StableWithinTier(t *testing.T) {
	grades := map[string]proxyURLGrade{
		"a-old:1080": {Score: 0.91, Decidable: true},
		"a-new:1080": {Score: 0.92, Decidable: true},
	}
	fetched := [][]string{
		{"a-old:1080", "a-new:1080"},
	}
	cands := collectRankedCandidates(fetched, grades)
	if cands[0].address != "a-old:1080" || cands[1].address != "a-new:1080" {
		t.Errorf("stable sort expected source order for same-tier, got %v", []string{cands[0].address, cands[1].address})
	}
}

// TestRankedProxyCandidates_UndecidableRanksLast pins that a decidable-zero
// (honeypot) ranks as F, and an undecidable pass ranks below every graded
// entry (it has no verdict, so it cannot be preferred over a graded F).
func TestRankedProxyCandidates_UndecidableRanksLast(t *testing.T) {
	grades := map[string]proxyURLGrade{
		"graded-f:1080":    {Score: 0.1, Decidable: true},
		"undecidable:1080": {}, // no verdict
	}
	fetched := [][]string{{"undecidable:1080", "graded-f:1080"}}
	cands := collectRankedCandidates(fetched, grades)
	if cands[0].address != "graded-f:1080" {
		t.Errorf("graded F must rank above undecidable, got %v first", cands[0].address)
	}
}
