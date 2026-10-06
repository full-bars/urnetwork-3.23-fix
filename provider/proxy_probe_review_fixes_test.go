package main

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests surfaced by the test-generation passes that the suite did not already
// have. Each is deterministic (fixed seeds, injected DNS, fake SOCKS servers, no
// sleeps, no live network) and falsifiable — the comment names the change that
// would turn it red.

// TestMinConfirmDials_GrowthLoopBoundsExactFloor pins BOTH bounds of the growth
// loop's abort: it must not convict below the floor, and it must not keep dialing
// past it. The existing growth test only asserts a lower bound, so a regression
// that dialled the whole adaptive block would still pass there.
//
// Falsifiable: drop `|| confirmNeeded(res, cfg)` from the growth trigger -> 4
// dials; drop `&& res.Total >= cfg.MinConfirmDials` from the growth abort -> 5;
// drop the abort entirely -> 8.
func TestMinConfirmDials_GrowthLoopBoundsExactFloor(t *testing.T) {
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.MinSampleWidth = 4
	cfg.SampleWidth = 8
	cfg.MaxSampleWidth = 8
	cfg.MinConfirmDials = 6
	cfg.PassBar = 0.60
	cfg.BorderlineBand = 0.15
	cfg.TargetTimeout = time.Second

	pass := tableProbePassCounter.Load()
	if n := seedProbeDNSForBlocks(t, addr, cfg, pass); n < 8 {
		t.Skipf("seeded %d hosts; need >= 8 for the base + growth blocks", n)
	}

	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	dials := int(connects.Load() - before)

	if dials != 6 {
		t.Fatalf("dialed %d, want exactly 6 (4 base + 2 growth to reach the floor)", dials)
	}
	if res.Total != 6 {
		t.Errorf("Total = %d, want 6", res.Total)
	}
	if res.SampleWidth != 6 {
		t.Errorf("SampleWidth = %d, want 6 (growth must stop at the floor, not the adaptive width)", res.SampleWidth)
	}
	if !res.Decidable || res.Score != 0 {
		t.Errorf("want a decidable 0.0 conviction, got decidable=%v score=%v", res.Decidable, res.Score)
	}
}

// TestMinConfirmDials_ResolverStarvationSuppressesGrowth drives the starvation
// guard through the REAL probe, not the pure predicate: a base the box could
// barely resolve must not trigger a growth block it cannot ask either.
//
// Falsifiable: remove `res.Total*2 < res.SampleWidth` from confirmNeeded -> the
// probe grows and dials past 2.
func TestMinConfirmDials_ResolverStarvationSuppressesGrowth(t *testing.T) {
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 12
	cfg.MinConfirmDials = 6
	cfg.TargetTimeout = time.Second

	pass := tableProbePassCounter.Load()
	base := sampleProbeHosts(tableProbeSeed(addr, pass), cfg.SampleWidth, cfg.UseSpreadOrder)
	if len(base) < 6 {
		t.Fatalf("table too small: got %d hosts", len(base))
	}
	resolvable := map[string]bool{}
	probeDNSCache.Lock()
	for i, h := range base {
		if i < 2 {
			resolvable[h] = true
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
		}
		delete(probeDNSCache.fail, h)
	}
	for _, h := range connect.ProbeHostNames() {
		if !resolvable[h] {
			delete(probeDNSCache.m, h)
			probeDNSCache.fail[h] = time.Now()
		}
	}
	probeDNSCache.Unlock()
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

	if dials != 2 {
		t.Fatalf("starved base dialed %d times, want 2 — a resolver-starved pass must not grow", dials)
	}
	if res.Total != 2 || res.SampleWidth != 6 {
		t.Errorf("Total=%d SampleWidth=%d, want 2 and 6 (no growth block)", res.Total, res.SampleWidth)
	}
	if res.Decidable {
		t.Errorf("a pass below the resolvable quorum must not be decidable")
	}
}

// TestMinConfirmDials_PartialSuccessSuppressesPrematureConviction exercises the
// viability numerator with a NON-ZERO OK, which the all-refused tests never do: a
// proxy that answered one destination and then failed a run must still be made to
// finish its block before it can be convicted.
//
// Falsifiable: drop `&& res.Total >= cfg.MinConfirmDials` from the base abort ->
// the floor=6 run stops at 4 dials like the floor=0 run.
func TestMinConfirmDials_PartialSuccessSuppressesPrematureConviction(t *testing.T) {
	// dial 1 answers, everything after refuses
	newServer := func() (string, func()) {
		addr, _, cleanup := listenSocks5Sequenced(t, func(n int) byte {
			if n == 1 {
				return 0x00
			}
			return 0x05
		})
		return addr, cleanup
	}

	base := defaultProxyTableProbeConfig()
	base.SampleWidth = 6
	base.MinSampleWidth = 6
	base.MaxSampleWidth = 12
	base.PassBar = 0.60
	base.BorderlineBand = 0.15
	base.TargetTimeout = time.Second

	// floor off: the bar goes unreachable at dial 4 (best = 3/6 = 0.50), so it aborts
	addr1, cleanup1 := newServer()
	defer cleanup1()
	cfg := base
	cfg.MinConfirmDials = 0
	seedProbeDNSForBlocks(t, addr1, cfg, tableProbePassCounter.Load())
	resNoFloor := probeTableThroughProxy(context.Background(), addr1, "", "", "", 0, cfg)
	if resNoFloor.Total != 4 || resNoFloor.OK != 1 {
		t.Errorf("floor=0: Total=%d OK=%d, want 4 and 1 (abort once the bar is unreachable)",
			resNoFloor.Total, resNoFloor.OK)
	}

	// floor on: the same pass must run its whole block
	addr2, cleanup2 := newServer()
	defer cleanup2()
	cfg = base
	cfg.MinConfirmDials = 6
	seedProbeDNSForBlocks(t, addr2, cfg, tableProbePassCounter.Load())
	res := probeTableThroughProxy(context.Background(), addr2, "", "", "", 0, cfg)
	if res.Total != 6 || res.OK != 1 {
		t.Errorf("floor=6: Total=%d OK=%d, want 6 and 1", res.Total, res.OK)
	}
	if res.Total != res.SampleWidth {
		t.Errorf("display overstates dials: SampleWidth=%d Total=%d", res.SampleWidth, res.Total)
	}
	if !res.Decidable {
		t.Errorf("want decidable")
	}
	if want := 1.0 / 6.0; math.Abs(res.Score-want) > 1e-9 {
		t.Errorf("Score = %v, want %v", res.Score, want)
	}
}

// TestProbeTableThroughProxy_HonoursSpreadOrderAndKillSwitch binds the config
// toggle to the live engine. Every sampler test asserts on the pure math, so
// hardcoding the flag at the call site would leave them all green.
//
// Falsifiable: hardcode cfg.UseSpreadOrder at the sampling call site -> one of the
// two host-order assertions fails.
func TestProbeTableThroughProxy_HonoursSpreadOrderAndKillSwitch(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x05)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MinConfirmDials = 6 // run the whole block so Failed is complete
	cfg.TargetTimeout = time.Second

	pass := tableProbePassCounter.Load()
	seed := tableProbeSeed(addr, pass)
	spreadHosts := sampleProbeHosts(seed, 6, true)
	legacyHosts := sampleProbeHosts(seed, 6, false)

	same := true
	for i := range spreadHosts {
		if spreadHosts[i] != legacyHosts[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatalf("precondition: the two orders are identical for seed %d", seed)
	}

	probeDNSCache.Lock()
	for _, h := range append(append([]string{}, spreadHosts...), legacyHosts...) {
		probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
		delete(probeDNSCache.fail, h)
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range append(append([]string{}, spreadHosts...), legacyHosts...) {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})

	cfg.UseSpreadOrder = true
	got := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if len(got.Failed) != 6 {
		t.Fatalf("spread pass: %d failed hosts, want 6", len(got.Failed))
	}
	for i, h := range spreadHosts {
		if got.Failed[i] != h {
			t.Errorf("spread order at %d: dialed %q, want %q", i, got.Failed[i], h)
		}
	}

	cfg.UseSpreadOrder = false
	got = probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if len(got.Failed) != 6 {
		t.Fatalf("legacy pass: %d failed hosts, want 6", len(got.Failed))
	}
	for i, h := range legacyHosts {
		if got.Failed[i] != h {
			t.Errorf("legacy order at %d: dialed %q, want %q", i, got.Failed[i], h)
		}
	}
}

// TestKillSwitch_ReproducesTheLegacyDialProfileIncludingTheAbort extends the
// host-set kill-switch test to the DIAL profile: with the switch off and no floor,
// an all-refused pass must fail fast exactly as it always did.
//
// Falsifiable: leave the spread sampler on with the switch off -> the abort
// boundary and the dialed hosts differ.
func TestKillSwitch_ReproducesTheLegacyDialProfileIncludingTheAbort(t *testing.T) {
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.UseSpreadOrder = false // the A/B lever
	cfg.MinConfirmDials = 0    // and no floor
	cfg.TargetTimeout = time.Second

	pass := tableProbePassCounter.Load()
	seedProbeDNSForBlocks(t, addr, cfg, pass)

	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	dials := int(connects.Load() - before)

	// legacy fail-fast: the bar is unreachable after 3 of 6 failures
	if dials != 3 {
		t.Errorf("legacy dial profile: %d dials, want 3 (fail-fast abort)", dials)
	}
	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	if res.SampleWidth != 6 {
		t.Errorf("SampleWidth = %d, want 6 (the intended block)", res.SampleWidth)
	}
	if !res.Decidable || res.Score != 0 {
		t.Errorf("want the legacy decidable 0.0 conviction, got decidable=%v score=%v", res.Decidable, res.Score)
	}
	// and the dialed hosts are the contiguous block's first three
	legacy := sampleProbeHosts(tableProbeSeed(addr, pass), 6, false)
	for i, h := range res.Failed {
		if h != legacy[i] {
			t.Errorf("legacy host at %d: dialed %q, want %q", i, h, legacy[i])
		}
	}
}
