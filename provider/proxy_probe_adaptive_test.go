package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests for the probe redesign (2026-08-23): adaptive sample growth, honest
// 'pending' status, per-tick probe budget, and the global dial rate limiter.
// All four are additive to the existing stage-1 model.

// seedProbeDNSForBlocks injects fake resolutions for BOTH the base block and
// the adaptive growth block of address at pass, so an offline probe can grow
// deterministically. Returns the total number of distinct hosts seeded; a
// caller that needs every sampled host resolvable can skip on a short count.
func seedProbeDNSForBlocks(t *testing.T, address string, cfg proxyTableProbeConfig, pass uint64) int {
	t.Helper()
	added := map[string]bool{}
	probeDNSCache.Lock()
	seed := func(blockSeed uint64, width int) {
		if width <= 0 {
			return
		}
		hosts, _ := connect.SampleProbeTargets(blockSeed, width)
		for _, h := range hosts {
			if !added[h] {
				added[h] = true
			}
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
			delete(probeDNSCache.fail, h)
		}
	}
	seed(tableProbeSeed(address, pass), cfg.SampleWidth)
	if cfg.MaxSampleWidth > cfg.SampleWidth {
		seed(tableProbeSeed(address, pass)+adaptiveBlockSeedOffset, cfg.MaxSampleWidth-cfg.SampleWidth)
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for h := range added {
			delete(probeDNSCache.m, h)
		}
	})
	return len(added)
}

// --- adaptive sample growth -----------------------------------------------

func TestGrowthNeeded_Table(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.PassBar = 0.6
	cfg.BorderlineBand = 0.15 // band [0.45, 0.75]
	cases := []struct {
		name  string
		ok    int
		total int
		want  bool
	}{
		{"clearly good above band", 10, 12, false},
		{"clearly good top edge", 9, 12, true}, // 0.75 inclusive
		{"borderline middle", 6, 12, true},     // 0.50
		{"borderline near bar", 7, 12, true},   // 0.583
		{"clearly dead below band", 2, 12, false},
		{"no attempts", 0, 0, false},
	}
	for _, c := range cases {
		res := tableProbeResult{OK: c.ok, Total: c.total}
		if got := growthNeeded(res, cfg); got != c.want {
			t.Errorf("growthNeeded(%d/%d) = %v, want %v", c.ok, c.total, got, c.want)
		}
	}
}

// TestProbeTableThroughProxy_NoGrowthForClearlyGood: all CONNECTs succeed so
// the score is 1.0, far above the borderline band; the probe must spend only
// the base block and not grow.
func TestProbeTableThroughProxy_NoGrowthForClearlyGood(t *testing.T) {
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.MaxSampleWidth = 12
	cfg.TargetTimeout = time.Second
	if n := seedProbeDNSForBlocks(t, addr, cfg, tableProbePassCounter.Load()); n < 4 {
		t.Skipf("only %d hosts seeded (DNS on this box); need >= 4 for a clean base", n)
	}
	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", cfg)
	after := connects.Load()
	if res.Score != 1.0 {
		t.Fatalf("expected score 1.0, got %v", res.Score)
	}
	if res.SampleWidth != 4 {
		t.Errorf("clearly-good proxy must not grow: SampleWidth=%d, want 4", res.SampleWidth)
	}
	if int(after-before) != 4 {
		t.Errorf("expected exactly %d dials (base block), got %d", 4, after-before)
	}
}

// TestProbeTableThroughProxy_GrowsForBorderline: a proxy that answers roughly
// half the targets lands a base score inside the borderline band, so the probe
// grows the sample to keep deciding instead of trusting a thin base verdict.
func TestProbeTableThroughProxy_GrowsForBorderline(t *testing.T) {
	// Base block of 6: succeed everywhere except the 1st and 6th CONNECT. That
	// finishes the base at 4/6 = 0.667 — inside [PassBar-0.15, PassBar+0.15]
	// AND safely above the 0.6 viability abort — so the probe is genuinely
	// borderline and must grow to be sure. If it aborted or scored outside the
	// band the test's point (grows for the uncertain middle) would be moot.
	repFor := func(n int) byte {
		if n == 1 || n == 6 {
			return 0x05
		}
		return 0x00
	}
	addr, connects, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MaxSampleWidth = 12
	cfg.PassBar = 0.6
	cfg.BorderlineBand = 0.15
	cfg.TargetTimeout = time.Second
	pass := tableProbePassCounter.Load()
	if n := seedProbeDNSForBlocks(t, addr, cfg, pass); n < 12 {
		t.Skipf("seeded %d hosts; need full 12 for a deterministic growth assertion", n)
	}
	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", cfg)
	after := connects.Load()
	if res.SampleWidth <= cfg.SampleWidth {
		t.Errorf("borderline proxy should grow beyond base: SampleWidth=%d, base=%d", res.SampleWidth, cfg.SampleWidth)
	}
	if !res.Decidable || res.Total <= 0 {
		t.Errorf("grown pass should remain decidable and attempted: decidable=%v total=%d", res.Decidable, res.Total)
	}
	if int(after-before) != res.SampleWidth {
		t.Errorf("dial count %d should match SampleWidth %d", after-before, res.SampleWidth)
	}
}

// --- honest 'pending' status ----------------------------------------------

// TestPaidGrader_SetsPendingOnReachableUndecidable: a paid proxy the probe
// REACHED (at least one CONNECT dialed through it) but could not decide
// (fewer than half the sample resolvable from this box, e.g. DNS-gutted)
// must be marked Pending=true with NO grade — the operator sees "could not
// evaluate from this box", never a fabricated tier.
func TestPaidGrader_SetsPendingOnReachableUndecidable(t *testing.T) {
	withTempHome(t)
	// A reachable fake proxy: answers every CONNECT. But only a THIN subset of
	// the sampled hosts resolve on this box (resolver gutted), so the pass
	// reaches the proxy yet produces no decidable verdict -> Pending.
	addr, _, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	writePaidGradeProbeOverride(t, true)

	src := filepathJoinHome(t, "paid.txt") // see helper below
	os.WriteFile(src, []byte(addr+":u:p\n"), 0600)
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 3, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Force the probe to see only 1 resolvable host of the base block (quorum
	// for the base is reached none). We clear the fail-cache and seed a single
	// base host, so Total>0 but resolvable < half -> undecidable.
	seedOnlyOneProbeHost(t, addr)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	if !e.Pending {
		t.Errorf("reachable-but-undecidable paid proxy must be Pending: %+v", e)
	}
	if e.Graded {
		t.Errorf("pending proxy must NOT be graded: %+v", e)
	}
	if !e.LastGraded.After(time.Now().Add(-time.Minute)) {
		t.Errorf("LastGraded must advance even on a pending pass: %+v", e)
	}
}

// TestPaidGrader_ClearsPendingOnDecidable: once a later pass IS decidable, the
// pending flag is cleared (a real grade replaces "could not evaluate").
func TestPaidGrader_ClearsPendingOnDecidable(t *testing.T) {
	withTempHome(t)
	addr, _, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	writePaidGradeProbeOverride(t, true)
	src := filepathJoinHome(t, "paid.txt")
	os.WriteFile(src, []byte(addr+":u:p\n"), 0600)
	// Start with a proxy that is currently Pending=true (e.g. from an earlier
	// DNS-gutted pass) and fully resolvable now.
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 4, Health: "up", Source: "file", Pending: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 99)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	if e.Pending {
		t.Errorf("a decidable pass must clear Pending: %+v", e)
	}
	if !e.Graded {
		t.Errorf("a decidable pass must grade: %+v", e)
	}
}

// --- per-tick probe budget ------------------------------------------------

func TestApplyPaidProbeBudget_Basic(t *testing.T) {
	now := time.Now()
	mk := func(addr string, at time.Time) gradeTarget { return gradeTarget{addr: addr, snapshotGradedAt: at} }
	targets := []gradeTarget{
		mk("fresh", now),                 // just probed
		mk("old", now.Add(-3*time.Hour)), // very stale
		mk("never", time.Time{}),         // never graded
	}
	got := applyPaidProbeBudget(targets, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 kept, got %d", len(got))
	}
	// oldest-stale-first: never-graded first, then the oldest timestamp.
	if got[0].addr != "never" || got[1].addr != "old" {
		t.Errorf("want never-graded then oldest, got %s, %s", got[0].addr, got[1].addr)
	}
	// The capped-out (freshest) target is deferred.
	if len(got) == 3 {
		t.Error("budget must cap the list")
	}
}

func TestApplyPaidProbeBudget_DisabledWhenZero(t *testing.T) {
	now := time.Now()
	targets := []gradeTarget{
		{addr: "a", snapshotGradedAt: now},
		{addr: "b", snapshotGradedAt: now.Add(-time.Hour)},
	}
	if got := applyPaidProbeBudget(targets, 0); len(got) != 2 {
		t.Errorf("budget<=0 disables the cap: got %d kept", len(got))
	}
}

// --- global dial rate limiter ---------------------------------------------

func TestGlobalProbeDialLimiter_Constants(t *testing.T) {
	if maxProbeDialsPerSec != 50 {
		t.Errorf("maxProbeDialsPerSec = %d, want 50", maxProbeDialsPerSec)
	}
	if maxProbeDialBurst <= 0 {
		t.Errorf("burst must be > 0, got %d", maxProbeDialBurst)
	}
}

// ---------- helpers -------------------------------------------------------

// filepathJoinHome joins a filename under the temp home set by withTempHome.
func filepathJoinHome(t *testing.T, name string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, name)
}

// seedOnlyOneProbeDNSHost makes the box resolve exactly ONE host of the base
// block (plus nothing else), so a probe REACHES the proxy (Total>0) but
// cannot reach the decidable quorum (half the sample) — the DNS-gutted box a
// 'pending' status exists to represent. The resolveProbeTarget fail-cache is
// cleared for every base host, but only one is given a resolution.
func seedOnlyOneProbeDNSHost(t *testing.T, address string) {
	t.Helper()
	cfg := resolveProxyTableProbeConfig()
	pass := tableProbePassCounter.Load()
	hosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), cfg.SampleWidth)
	probeDNSCache.Lock()
	// Clear any prior fail-cache for every base host so none short-circuit.
	for _, h := range hosts {
		delete(probeDNSCache.fail, h)
	}
	if len(hosts) > 0 {
		probeDNSCache.m[hosts[0]] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
	} else {
		for _, h := range hosts {
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
		}
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range hosts {
			delete(probeDNSCache.m, h)
		}
	})
}

// ---------- helpers -------------------------------------------------------

// seedOnlyOneProbeHost makes the box resolve exactly ONE host of the base
// block (plus nothing else), so a probe REACHES the proxy (Total>0) but cannot
// reach the decidable quorum (half the sample) — the DNS-gutted box a 'pending'
// status exists to represent. The resolveProbeTarget fail-cache is cleared for
// every base host, but only the first is given a resolution.
func seedOnlyOneProbeHost(t *testing.T, address string) {
	t.Helper()
	cfg := resolveProxyTableProbeConfig()
	pass := tableProbePassCounter.Load()
	// Collect the full base+growth block so every host the grader might dial
	// is accounted and isolated from any prior test's DNS cache state.
	var hosts []string
	for _, w := range []int{cfg.SampleWidth, cfg.MaxSampleWidth} {
		if w <= 0 {
			continue
		}
		h, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), w)
		hosts = append(hosts, h...)
	}
	probeDNSCache.Lock()
	for _, h := range hosts {
		delete(probeDNSCache.m, h)
		delete(probeDNSCache.fail, h)
	}
	if len(hosts) > 0 {
		// Exactly one resolvable host -> the probe REACHES the proxy but the
		// box cannot resolve the rest of the sample (DNS-gutted): Total>0 yet
		// below the decidable quorum. All other hosts fast-fail via the
		// fail-cache (probeDNSFailTTL) without touching the network.
		probeDNSCache.m[hosts[0]] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
		for _, h := range hosts[1:] {
			probeDNSCache.fail[h] = time.Now()
		}
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range hosts {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})
}
