package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// MinConfirmDials is a floor on ATTEMPTED dials (res.Total), not on
// SampleWidth. A sub-bar pass below the floor runs its whole block before the
// viability abort may convict it, so a correlated region-deny cluster cannot
// seal an F from a handful of dials. 0 (the URL admission path) must reproduce
// the previous fail-fast behaviour byte for byte.

// TestMinConfirmDials_SubBarPassRunsWholeBlockBeforeConviction is the core
// regression: the same all-refused pass must fail fast with the floor off and
// dial its whole block with the floor on.
func TestMinConfirmDials_SubBarPassRunsWholeBlockBeforeConviction(t *testing.T) {
	// every CONNECT refused, so the bar is unreachable from the first dial —
	// exactly the case the fail-fast abort was built for.
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 12
	cfg.TargetTimeout = time.Second

	if n := seedProbeDNSForBlocks(t, addr, cfg, tableProbePassCounter.Load()); n < 6 {
		t.Skipf("seeded %d hosts; need >= 6 for a deterministic block", n)
	}

	// floor disabled -> fail-fast, unchanged.
	cfg.MinConfirmDials = 0
	before := connects.Load()
	probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	noFloor := int(connects.Load() - before)
	if noFloor >= 6 {
		t.Errorf("MinConfirmDials=0 must fail fast: dialed %d of 6", noFloor)
	}

	// floor enabled -> the same pass must run the whole block.
	cfg.MinConfirmDials = 6
	before = connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	floorDials := int(connects.Load() - before)
	if floorDials != 6 {
		t.Errorf("MinConfirmDials=6 must dial the whole 6-wide block: dialed %d", floorDials)
	}

	// the conviction must rest on the floor, measured in ATTEMPTED dials.
	if res.Total != 6 {
		t.Errorf("conviction Total = %d, want 6 (the floor counts attempted dials)", res.Total)
	}
	// and the operator-facing pair must stop overstating the work: the abort
	// used to leave Total=3 against SampleWidth=6, which printed as "0/6".
	if res.Total != res.SampleWidth {
		t.Errorf("display still overstates dials: SampleWidth=%d Total=%d", res.SampleWidth, res.Total)
	}
	if !res.Decidable || res.Score != 0 {
		t.Errorf("expected a decidable 0.0 conviction, got decidable=%v score=%v", res.Decidable, res.Score)
	}
}

// TestConfirmNeeded_Table pins the trigger's boundary: only a below-band pass
// that has not yet met the floor grows. In-band passes belong to growthNeeded
// and above-band passes have nothing to confirm.
func TestConfirmNeeded_Table(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.PassBar = 0.6
	cfg.BorderlineBand = 0.15 // below-band means score < 0.45

	cases := []struct {
		name      string
		total, ok int
		floor     int
		want      bool
	}{
		{"disabled", 3, 0, 0, false},
		{"sub-bar below floor grows", 3, 0, 6, true},
		{"one short of floor grows", 5, 0, 6, true},
		{"floor met does not grow", 6, 0, 6, false},
		{"in-band is growthNeeded's job", 3, 2, 6, false}, // 0.667
		{"above band has nothing to confirm", 2, 2, 6, false},
		{"nothing asked", 0, 0, 6, false},
		{"starved base does not grow", 2, 0, 6, false}, // only 2 of 6 attempted
		{"half-attempted base still grows", 3, 0, 6, true},
	}
	for _, c := range cases {
		res := tableProbeResult{Total: c.total, OK: c.ok, SampleWidth: 6}
		cfg.MinConfirmDials = c.floor
		if got := confirmNeeded(res, cfg); got != c.want {
			t.Errorf("%s: confirmNeeded(total=%d ok=%d floor=%d) = %v, want %v",
				c.name, c.total, c.ok, c.floor, got, c.want)
		}
	}
}

// TestMinConfirmDials_ConfirmationGrowthIsSizedToTheShortfall pins the dial
// budget: a pass that fell ONE dial short of the floor must grow by one dial, not
// to the full adaptive width. Expanding to probeWidth (30 extra here) spends ~30
// dials on a proxy that is already failing — at a 4s target timeout that is
// minutes per dead proxy inside a sweep.
//
// Falsifiable: remove the shortfall sizing and this pass dials ~14 times instead
// of 6.
func TestMinConfirmDials_ConfirmationGrowthIsSizedToTheShortfall(t *testing.T) {
	withTempHome(t)
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 36 // room to grow: the trap only bites with a wide ceiling
	cfg.UseSpreadOrder = true
	cfg.MinConfirmDials = 6
	cfg.TargetTimeout = 300 * time.Millisecond

	// exactly ONE unresolvable base host: 5 attempted, all refused, so the pass
	// lands one dial short of the floor
	base := sampleProbeHosts(tableProbeSeed(addr, tableProbePassCounter.Load()), cfg.MinSampleWidth, true)
	probeDNSCache.Lock()
	for i, h := range base {
		if i == 0 {
			delete(probeDNSCache.m, h)
			probeDNSCache.fail[h] = time.Now()
		} else {
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("198.51.100.1"), at: time.Now()}
			delete(probeDNSCache.fail, h)
		}
	}
	probeDNSCache.Unlock()
	// seed the whole growth block, so every growth dial is attemptable and the
	// dial count measures the SIZING, not resolution luck
	for _, h := range disjointGrowthHosts(addr, tableProbePassCounter.Load(), cfg.MinSampleWidth, 30, true) {
		probeDNSCache.Lock()
		probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("198.51.100.1"), at: time.Now()}
		delete(probeDNSCache.fail, h)
		probeDNSCache.Unlock()
	}
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range connect.ProbeHostNames() {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})

	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	dials := int(connects.Load() - before)

	if res.Total != cfg.MinConfirmDials {
		t.Errorf("Total = %d, want exactly the floor %d", res.Total, cfg.MinConfirmDials)
	}
	if dials > cfg.MinConfirmDials+1 {
		t.Errorf("dialed %d times to cover a 1-dial shortfall (floor %d) — the confirmation growth "+
			"expanded to the adaptive width instead of the shortfall", dials, cfg.MinConfirmDials)
	}
}

// TestClampConfirmFloor_BoundsEveryWriter pins the shared clamp directly. The
// paid grader overrides the resolved config AFTER the loader clamp, so it must
// call this too — clamping only in the loader leaves the paid path with an
// unsatisfiable floor and a silently disabled fail-fast abort.
func TestClampConfirmFloor_BoundsEveryWriter(t *testing.T) {
	cases := []struct {
		sampleWidth, maxSampleWidth, floor, want int
	}{
		{6, 6, 6, 6},     // exactly the widest block: allowed
		{4, 4, 6, 4},     // the paid override on a narrow config: clamped
		{12, 36, 6, 6},   // room to spare: untouched
		{12, 36, 99, 36}, // above the ceiling: clamped to probeWidth
	}
	for _, c := range cases {
		cfg := proxyTableProbeConfig{SampleWidth: c.sampleWidth, MaxSampleWidth: c.maxSampleWidth, MinConfirmDials: c.floor}
		clampConfirmFloor(&cfg)
		if cfg.MinConfirmDials != c.want {
			t.Errorf("sampleWidth=%d max=%d floor=%d: got %d, want %d",
				c.sampleWidth, c.maxSampleWidth, c.floor, cfg.MinConfirmDials, c.want)
		}
	}
}

// TestMinConfirmDials_FloorCannotExceedWhatTheProbeDials guards the clamp
// through the REAL resolver, and pins the order of the clamps: the floor must be
// bounded by the post-clamp widths, so a wide sample_width override cannot leave
// an unsatisfiable floor behind (which would make the pass grow on every sweep
// and still convict on thin evidence).
func TestMinConfirmDials_FloorCannotExceedWhatTheProbeDials(t *testing.T) {
	withTempHome(t)
	writeReviewProbeOverride(t, map[string]any{"sample_width": 1000, "min_confirm_dials": 500})
	cfg := loadProxyTableProbeConfig()
	if cfg.MinConfirmDials > cfg.probeWidth() {
		t.Fatalf("floor %d exceeds probeWidth %d — the floor clamp ran before the width clamps",
			cfg.MinConfirmDials, cfg.probeWidth())
	}
}
