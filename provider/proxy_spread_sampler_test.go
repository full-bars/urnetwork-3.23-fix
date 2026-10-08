package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// Tests for the content-keyed spread sampler (proxy_spread_sampler.go).
//
// The invariants below are the ones the probe's correctness rests on, so they
// are pinned explicitly rather than assumed: block disjointness across
// consecutive passes, whole-table coverage in one cycle, the clamp algebra, and
// the growth-stride disjointness proof.

// spread order is exactly the table sorted by fnv64a(hostname) — a strict
// content key, independent of the authored row order.
func TestSpreadOrderHosts_IsContentKeyedSorted(t *testing.T) {
	order := spreadOrderHosts()
	table := connect.ProbeHostNames()
	if len(order) != len(table) {
		t.Fatalf("spread order length %d != table length %d", len(order), len(table))
	}
	// same multiset of hosts
	seen := map[string]int{}
	for _, h := range table {
		seen[h]++
	}
	for _, h := range order {
		seen[h]--
	}
	for h, n := range seen {
		if n != 0 {
			t.Fatalf("host %q count mismatch after permutation (%d)", h, n)
		}
	}
	// strictly ascending by (hash, host)
	for i := 1; i < len(order); i++ {
		hi, hj := hostHash(order[i-1]), hostHash(order[i])
		if hi > hj || (hi == hj && order[i-1] >= order[i]) {
			t.Fatalf("spread order not strictly sorted at %d: %q(%d) then %q(%d)",
				i, order[i-1], hi, order[i], hj)
		}
	}
	// cached: same slice on a second call
	if &order[0] != &spreadOrderHosts()[0] {
		t.Fatalf("spread order is not cached")
	}
}

// consecutive seeds must produce pairwise-disjoint blocks (the property that
// makes the rotation honest), and sampling must be deterministic.
func TestSampleProbeSpread_ConsecutivePassesDisjoint(t *testing.T) {
	const width = 6
	total := len(spreadOrderHosts())
	var prev []string
	for seed := uint64(0); seed < 20; seed++ {
		got := sampleProbeSpread(seed, width)
		if len(got) != width {
			t.Fatalf("seed %d: got %d hosts, want %d", seed, len(got), width)
		}
		// deterministic
		again := sampleProbeSpread(seed, width)
		for i := range got {
			if got[i] != again[i] {
				t.Fatalf("seed %d not deterministic at %d: %q vs %q", seed, i, got[i], again[i])
			}
		}
		if prev != nil {
			inPrev := map[string]bool{}
			for _, h := range prev {
				inPrev[h] = true
			}
			for _, h := range got {
				if inPrev[h] {
					t.Fatalf("seed %d block overlaps seed %d on %q", seed, seed-1, h)
				}
			}
		}
		prev = got
	}
	_ = total
}

// one full cycle of passes must cover every host in the table.
func TestSampleProbeSpread_CoversWholeTableInOneCycle(t *testing.T) {
	const width = 6
	total := len(spreadOrderHosts())
	seen := map[string]bool{}
	passes := total/width + 2
	for seed := uint64(0); seed < uint64(passes); seed++ {
		for _, h := range sampleProbeSpread(seed, width) {
			seen[h] = true
		}
	}
	if len(seen) != total {
		t.Fatalf("cycle covered %d/%d hosts", len(seen), total)
	}
}

// clamp / degenerate inputs.
func TestSampleProbeSpread_ClampAndEdges(t *testing.T) {
	total := len(spreadOrderHosts())
	if got := sampleProbeSpread(7, 0); got != nil {
		t.Fatalf("n=0 should return nil, got %d hosts", len(got))
	}
	if got := sampleProbeSpread(7, -3); got != nil {
		t.Fatalf("n<0 should return nil, got %d hosts", len(got))
	}
	if got := sampleProbeSpread(7, total+50); len(got) != total {
		t.Fatalf("n>total should clamp to %d, got %d", total, len(got))
	}
	if got := sampleProbeSpread(7, total); len(got) != total {
		t.Fatalf("n=total should return the whole table, got %d", len(got))
	}
}

// the growth strides must stay disjoint from the base block (the
// disjointGrowthHosts proof, restated on the permuted axis).
func TestSampleProbeSpread_GrowthStridesDisjoint(t *testing.T) {
	const width = 6
	for _, seed := range []uint64{0, 1, 5, 42, 99} {
		base := sampleProbeSpread(seed, width)
		inBase := map[string]bool{}
		for _, h := range base {
			inBase[h] = true
		}
		for _, step := range []uint64{1, 2, 3, 4, 5} {
			for _, h := range sampleProbeSpread(seed+step, width) {
				if inBase[h] {
					t.Fatalf("seed %d: growth stride +%d overlaps base on %q", seed, step, h)
				}
			}
		}
	}
}

// THE REGRESSION TEST. A region-deny gateway refuses a contiguous run of the
// authored table (a whole regional cluster). Under the legacy contiguous
// sampler such a run can occupy an ENTIRE block, which is the observed
// all-fail 0/6 -> decided F churn. Under the spread sampler the same run is
// scattered, so no block can be monopolised by it.
func TestSpreadSampler_RegionalDenyCannotMonopoliseABlock(t *testing.T) {
	table := connect.ProbeHostNames()
	const width = 6
	const region = 12 // a realistic regional tail (CN/RU/KR-JP/IN cluster size)

	// For each region position, build the deny set = `region` consecutive table
	// rows, then measure the WORST block for each sampler over many seeds.
	maxDeniedContiguous := 0
	maxDeniedSpread := 0

	indexOf := map[string]int{}
	for i, h := range table {
		indexOf[h] = i
	}

	for start := 0; start+region <= len(table); start += 3 {
		deny := map[string]bool{}
		for i := start; i < start+region; i++ {
			deny[table[i]] = true
		}

		for seed := uint64(0); seed < 60; seed++ {
			// legacy contiguous
			block, _ := connect.SampleProbeTargets(seed, width)
			n := 0
			for _, h := range block {
				if deny[h] {
					n++
				}
			}
			if n > maxDeniedContiguous {
				maxDeniedContiguous = n
			}

			// spread
			n = 0
			for _, h := range sampleProbeSpread(seed, width) {
				if deny[h] {
					n++
				}
			}
			if n > maxDeniedSpread {
				maxDeniedSpread = n
			}
		}
	}

	t.Logf("worst block: contiguous denied=%d/%d, spread denied=%d/%d",
		maxDeniedContiguous, width, maxDeniedSpread, width)

	if maxDeniedContiguous != width {
		t.Fatalf("precondition failed: expected the contiguous sampler to be able to "+
			"fill a whole block from a %d-row region, worst was %d", region, maxDeniedContiguous)
	}
	if maxDeniedSpread >= width {
		t.Fatalf("spread sampler let a %d-row contiguous region fill a whole %d-wide block "+
			"(worst %d) — the churn fix does not hold", region, width, maxDeniedSpread)
	}
}

// TestDisjointGrowthHosts_SpreadStrideDisjoint covers the spread=true growth
// stride. The legacy disjointness test pins only the contiguous order, so the
// permuted axis the default config now uses had no direct unit test.
func TestDisjointGrowthHosts_SpreadStrideDisjoint(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MaxSampleWidth = 12
	extra := cfg.MaxSampleWidth - cfg.SampleWidth
	for p := 0; p < 400; p++ {
		seed := tableProbeSeed("1.2.3.4:1080", uint64(p))
		base := sampleProbeHosts(seed, cfg.SampleWidth, true)
		baseSet := map[string]bool{}
		for _, h := range base {
			baseSet[h] = true
		}
		grown := disjointGrowthHosts("1.2.3.4:1080", uint64(p), cfg.SampleWidth, extra, true)
		if len(grown) != extra {
			t.Fatalf("pass %d: growth returned %d hosts, want %d", p, len(grown), extra)
		}
		seen := map[string]bool{}
		for _, h := range grown {
			if baseSet[h] {
				t.Errorf("pass %d: growth host %q collides with the spread base", p, h)
			}
			if seen[h] {
				t.Errorf("pass %d: duplicate host %q inside the growth block", p, h)
			}
			seen[h] = true
		}
	}
}

// the kill switch must reproduce the legacy behaviour exactly.
func TestSampleProbeHosts_KillSwitchMatchesLegacy(t *testing.T) {
	for _, seed := range []uint64{0, 3, 77} {
		want, _ := connect.SampleProbeTargets(seed, 6)
		got := sampleProbeHosts(seed, 6, false)
		if len(got) != len(want) {
			t.Fatalf("seed %d: legacy dispatch returned %d hosts, want %d", seed, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("seed %d: kill switch diverged at %d: %q vs %q", seed, i, got[i], want[i])
			}
		}
	}
}

// sanity: the spread order is not simply the authored order (otherwise nothing
// would have changed).
func TestSpreadOrderHosts_DiffersFromAuthoredOrder(t *testing.T) {
	order := spreadOrderHosts()
	table := connect.ProbeHostNames()
	same := 0
	for i := range order {
		if order[i] == table[i] {
			same++
		}
	}
	if same == len(table) {
		t.Fatalf("spread order is identical to the authored order — sampling was not spread")
	}
	if same > len(table)/2 {
		t.Logf("note: %d/%d positions coincide by chance", same, len(table))
	}
}
