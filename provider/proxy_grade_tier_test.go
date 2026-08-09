package main

import "testing"

// TestProxyGradeTier_Bands pins the A-F letter-grade mapping:
// A >= 0.9, B >= 0.8, C >= 0.7, D >= 0.6, F < 0.6.
func TestProxyGradeTier_Bands(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.0, "A"},
		{0.9, "A"},
		{0.899, "B"},
		{0.8, "B"},
		{0.79, "C"},
		{0.7, "C"},
		{0.69, "D"},
		{0.6, "D"},
		{0.599, "F"},
		{0.0, "F"},
	}
	for _, c := range cases {
		if got := proxyGradeTier(c.score); got != c.want {
			t.Errorf("proxyGradeTier(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}

// TestProxyGradeTier_Undecidable pins that an undecidable pass (no genuine
// verdict) has no tier at all — it must not be graded as F.
func TestProxyGradeTier_Undecidable(t *testing.T) {
	res := tableProbeResult{Score: 0, OK: 0, Total: 0, SampleWidth: 12, Decidable: false}
	if got := gradeTierFor(res); got != "" {
		t.Errorf("undecidable pass must have no tier, got %q", got)
	}
}

// TestProxyGradeTier_DecidableZero pins that a decidable 0.0 (honeypot:
// answered nothing of what it was asked) IS graded F — zero is a verdict.
func TestProxyGradeTier_DecidableZero(t *testing.T) {
	res := tableProbeResult{Score: 0, OK: 0, Total: 8, SampleWidth: 12, Decidable: true}
	if got := gradeTierFor(res); got != "F" {
		t.Errorf("decidable zero must be F, got %q", got)
	}
}

// TestTierRankOrder pins the priority ordering used for cap eviction:
// A (4) > B (3) > C (2) > D (1) > F (0) > "" (undecidable, lowest).
func TestTierRankOrder(t *testing.T) {
	order := []string{"A", "B", "C", "D", "F", ""}
	prev := 99
	for _, tier := range order {
		rank := proxyTierRank(tier)
		if rank >= prev {
			t.Errorf("tier %q rank %d must be strictly less than previous %d", tier, rank, prev)
		}
		prev = rank
	}
	if proxyTierRank("A") != proxyTierRank("a") {
		t.Error("tier rank must be case-insensitive")
	}
}
