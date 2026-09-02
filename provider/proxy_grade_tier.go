package main

import (
	"sort"
)

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
// eviction and prioritization always prefer graded entries. Only the
// uppercase letters proxyGradeTier produces are reachable: every
// production caller feeds this the output of proxyGradeTier, which returns
// only "A"-"F" (finding LOW — the lowercase branches were dead code).
func proxyTierRank(tier string) int {
	switch tier {
	case "A":
		return 4
	case "B":
		return 3
	case "C":
		return 2
	case "D":
		return 1
	case "F":
		return 0
	default:
		return -1
	}
}

// rankedProxyCandidate is one URL-source line with its tier rank, pooled
// across all sources for best-overall admission.
type rankedProxyCandidate struct {
	address string
	line    string
	rank    int
	grade   proxyURLGrade
	// hasGrade is true when the grade map contained a genuine verdict for
	// this address (decidable or not); used to attach the persisted grade.
	hasGrade bool
}

// rankFromGrade computes the tier rank for a stage-1 grade: decidable,
// non-socks5-only grades rank A=4..F=0 by letter; everything else (socks5-only,
// undecidable, ungraded) ranks -1. Used by BOTH the funnel's candidate
// ranking (collectRankedCandidates) and the merge's candidate-rank closure so
// the two can never drift (self-review finding).
func rankFromGrade(g proxyURLGrade) int {
	if g.Decidable && !g.Socks5Only {
		return proxyTierRank(proxyGradeTier(g.Score))
	}
	return -1
}

// collectRankedCandidates pools every parseable line from all sources with
// its tier rank and sorts best-first (A, B, C, D, F, then ungraded/
// socks5-only). The sort is STABLE within a tier, so same-tier candidates
// keep source order. This ordering is what makes cache admission a true
// A→B→C→D funnel: the merge fills from the front, so the highest-tier
// proxies across ALL sources get the slots first, and lower tiers fill
// whatever remains.
func collectRankedCandidates(fetched [][]string, grades map[string]proxyURLGrade) []rankedProxyCandidate {
	var cands []rankedProxyCandidate
	for _, lines := range fetched {
		for _, line := range lines {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			c := rankedProxyCandidate{address: address, line: line, rank: -1}
			if g, ok := grades[address]; ok {
				c.hasGrade = true
				c.grade = g
				c.rank = rankFromGrade(g)
			}
			cands = append(cands, c)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].rank > cands[j].rank
	})
	return cands
}

// cachedProxyAddresses returns the set of addresses already in the cache.
// The fetch cycle probes ONLY addresses not in this set — a cached proxy
// already has a grade; re-probing it on every cycle (or even hourly) is
// redundant because most don't change. Quality refresh of cached entries is
// the reaper's job (its stale sweep re-probes once-good entries after
// 1-3h). New addresses always get the full table probe at admission.
func cachedProxyAddresses(state *ProxyURLState) map[string]bool {
	addrs := map[string]bool{}
	if state == nil {
		return addrs
	}
	for addr := range state.Cache {
		addrs[addr] = true
	}
	return addrs
}

// mustReadProxyURLState reads proxy_url.json without failing the caller: on
// any error it returns an empty state (the fetch cycle then treats every
// address as new and probes it — safe, just slightly more probing than
// necessary for one cycle).
func mustReadProxyURLState() *ProxyURLState {
	state, err := readProxyURLState()
	if err != nil {
		// A corrupt or unreadable proxy_url.json must not look identical to
		// a fresh install: log it, or every cached address is treated as new
		// and re-probed on every cycle with no signal to the operator.
		tlog("[proxy][url] warning: could not read proxy_url.json for cached-address snapshot: %v (probing all addresses this cycle)\n", err)
		return &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}
	return state
}
