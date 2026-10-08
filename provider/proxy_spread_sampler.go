package main

import (
	"hash/fnv"
	"sort"
	"sync"

	"github.com/urnetwork/connect"
)

// Spread (content-keyed permutation) sampling for the stage-1 table probe.
//
// WHY. connect.SampleProbeTargets draws a CONTIGUOUS block of the health-host
// table — n consecutive rows starting at (seed*n)%total. The table is authored
// as a thematic catalog (connectivity/captive-portal, CDNs, search, social,
// streaming, e-commerce, Western news, then China/Russia/Korea-Japan/India ...),
// so a contiguous block lands on one or two adjacent themes. Destination
// failures are strongly correlated WITHIN a theme — a gateway that refuses
// CN/KR destinations fails that whole region at once — which collapses the
// effective sample size to ~1. A pass is then all-pass or all-fail depending
// only on WHICH region the rotation happened to land on, and consecutive passes
// rotate to different regions: the observed A<->F churn on paid proxies whose
// real capability never changed. Verified empirically — the recorded failure
// sets are literally runs of consecutive table indices (88/89/90 and
// 107/108/109).
//
// FIX. Sample blocks of a FIXED PERMUTATION of the table, ordered by
// fnv64a(hostname) — content-keyed, not position-keyed. Thematic adjacency is
// destroyed so one block spans many themes, while every property the probe
// relies on is preserved unchanged, because it is the SAME block tiling applied
// to a reordered axis:
//
//   - consecutive-seed blocks stay pairwise disjoint (start advances by n);
//   - the whole table is still walked in ceil(total/n) passes;
//   - the 2n <= total clamp algebra is unchanged;
//   - sampling stays deterministic and reproducible from the seed;
//   - and it is now robust to an upstream table REORDER, because the order is a
//     function of hostname content, never of row index. The fork does not own
//     the table — it is upstream-parity and shared with clients — so keying on
//     content rather than position is the only safe basis.
//
// DIAL COUNT IS IDENTICAL to the contiguous sampler for every width, so this
// changes no bandwidth profile; it only changes WHICH hosts a block contains.
//
// Kill switch: cfg.UseSpreadOrder == false restores the exact historical
// contiguous-block behaviour, byte for byte, which also makes it a clean A/B
// for measuring churn before and after on a canary box.

var probeSpreadOrder struct {
	mu    sync.Mutex
	hosts []string
}

// hostHash is the fixed content key for a hostname.
func hostHash(host string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(host))
	return h.Sum64()
}

// spreadOrderHosts returns the host table in a fixed, content-keyed order:
// ascending fnv64a(hostname), tie-broken by hostname so the result is a strict
// total order and therefore independent of the input slice's order. Cached and
// rebuilt only when the table size changes (an upstream edit that keeps the
// same length KEEPS SERVING THE CACHED PERMUTATION, because the guard compares
// length only and never content. That is safe today: probeHostNames is a
// compile-time constant with no runtime writer, so the table cannot change
// under a running process at all. If the table ever becomes runtime-mutable,
// this must validate membership (e.g. a hash sum) instead of length.
func spreadOrderHosts() []string {
	probeSpreadOrder.mu.Lock()
	defer probeSpreadOrder.mu.Unlock()

	// Length check FIRST: connect.ProbeHostNames() copies the whole table, so the
	// cached path must not allocate a copy it is about to discard. This runs once
	// per sampled block, under this mutex, from up to scaledProbeConcurrency
	// concurrent probes.
	if n := connect.ProbeHostCount(); probeSpreadOrder.hosts != nil && len(probeSpreadOrder.hosts) == n {
		return probeSpreadOrder.hosts
	}
	t := connect.ProbeHostNames() // copy — never mutate the package-owned table
	sort.Slice(t, func(i, j int) bool {
		hi, hj := hostHash(t[i]), hostHash(t[j])
		if hi != hj {
			return hi < hj
		}
		return t[i] < t[j]
	})
	probeSpreadOrder.hosts = t
	return probeSpreadOrder.hosts
}

// sampleProbeSpread mirrors connect.SampleProbeTargets' block math exactly
// (contiguous block of width n, start = (seed*n) % total) but over the
// content-keyed spread order instead of the authored row order. The returned
// slice is a fresh allocation; callers may retain it.
func sampleProbeSpread(seed uint64, n int) []string {
	order := spreadOrderHosts()
	total := uint64(len(order))
	if n <= 0 || total == 0 {
		return nil
	}
	if uint64(n) > total {
		n = int(total)
	}
	start := (seed * uint64(n)) % total
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, order[(start+uint64(i))%total])
	}
	return out
}

// sampleProbeHosts is the single dispatch point for the probe's target
// sampling, so one config switch covers every call site. spread=false is the
// legacy contiguous path (the resolver return value is unused by the provider).
func sampleProbeHosts(seed uint64, n int, spread bool) []string {
	if spread {
		return sampleProbeSpread(seed, n)
	}
	hosts, _ := connect.SampleProbeTargets(seed, n)
	return hosts
}
