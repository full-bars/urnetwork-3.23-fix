package main

// Letter-grade tiers for stage-1 probe scores. The mapping is deliberately
// fixed (not config-driven): A >= 0.9, B >= 0.8, C >= 0.7, D >= 0.6,
// F < 0.6. Tiers exist so an operator can see the score DISTRIBUTION of
// what the URL sources actually deliver (how many truly-good proxies vs
// merely-qualified vs junk) and prioritize the highest tiers, rather than
// a single pass/fail cut.

// proxyGradeTier maps a score to its letter grade.
func proxyGradeTier(score float64) string {
	switch {
	case score >= 0.9:
		return "A"
	case score >= 0.8:
		return "B"
	case score >= 0.7:
		return "C"
	case score >= 0.6:
		return "D"
	default:
		return "F"
	}
}

// gradeTierFor derives the tier for a probe result. An undecidable pass
// (no genuine verdict — cancelled, DNS-gutted, nothing asked) has NO tier:
// absence of evidence is not an F. A decidable pass is always graded,
// including a decidable 0.0 (the honeypot that answered nothing it was
// asked — that IS an F).
func gradeTierFor(res tableProbeResult) string {
	if !res.Decidable {
		return ""
	}
	return proxyGradeTier(res.Score)
}

// proxyTierRank returns a numeric priority for a tier: higher is better.
// The empty tier (undecidable / never graded) ranks below F so cap
// eviction and prioritization always prefer graded entries. Case-insensitive.
func proxyTierRank(tier string) int {
	switch tier {
	case "A", "a":
		return 4
	case "B", "b":
		return 3
	case "C", "c":
		return 2
	case "D", "d":
		return 1
	case "F", "f":
		return 0
	default:
		return -1
	}
}
