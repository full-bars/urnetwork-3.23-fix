package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/time/rate"
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
	// Mirror the probe's own derivations EXACTLY (Opus review TEST-1): the
	// staged probe dials the base block at baseW (= MinSampleWidth when
	// staging is active, else SampleWidth) and grows via disjoint SAME-WIDTH
	// strides to probeWidth(). Seeding any other width seeds a DIFFERENT
	// block entirely ((seed*n)%total), which made these tests silently depend
	// on live DNS instead of the seeded universe.
	baseW := cfg.MinSampleWidth
	if baseW <= 0 || baseW > cfg.SampleWidth {
		baseW = cfg.SampleWidth
	}
	growTo := cfg.probeWidth()

	added := map[string]bool{}
	seed := func(blockSeed uint64, width int) {
		if width <= 0 {
			return
		}
		hosts, _ := connect.SampleProbeTargets(blockSeed, width)
		for _, h := range hosts {
			added[h] = true
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
			delete(probeDNSCache.fail, h)
		}
	}
	probeDNSCache.Lock()
	seed(tableProbeSeed(address, pass), baseW)
	if growTo > baseW {
		for _, h := range disjointGrowthHosts(address, pass, baseW, growTo-baseW) {
			added[h] = true
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("93.184.216.34"), at: time.Now()}
			delete(probeDNSCache.fail, h)
		}
	}
	// Poison every OTHER table host into the fail-cache so a seeding mistake
	// fails loudly (Total collapses -> assertion fires) instead of silently
	// reaching live DNS and passing only on network-dependent boxes.
	allHosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), connect.ProbeHostCount())
	for _, h := range allHosts {
		if added[h] {
			continue
		}
		probeDNSCache.fail[h] = time.Now()
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for h := range added {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
		// Also clear the POISONED entries: they must not leak into later
		// tests (probeDNSFailTTL is 30s, longer than several tests).
		allHosts, _ := connect.SampleProbeTargets(tableProbeSeed(address, pass), connect.ProbeHostCount())
		for _, h := range allHosts {
			delete(probeDNSCache.fail, h)
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
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
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
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
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

func TestApplyPaidProbeBudget_TieBreakByAddr(t *testing.T) {
	// Equal staleness (all never-graded) must be ordered by address, NOT by the
	// randomized source-map iteration order (coderabbit review). Otherwise the
	// budget cut picks an arbitrary subset and a deferred proxy can starve
	// across ticks.
	targets := []gradeTarget{
		{addr: "c", snapshotGradedAt: time.Time{}},
		{addr: "a", snapshotGradedAt: time.Time{}},
		{addr: "b", snapshotGradedAt: time.Time{}},
	}
	got := applyPaidProbeBudget(targets, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 kept, got %d", len(got))
	}
	if got[0].addr != "a" || got[1].addr != "b" {
		t.Errorf("equal staleness must tie-break by addr ascending, got %s, %s", got[0].addr, got[1].addr)
	}
	// The tie-break must decide regardless of input order.
	reversed := []gradeTarget{
		{addr: "b", snapshotGradedAt: time.Time{}},
		{addr: "c", snapshotGradedAt: time.Time{}},
		{addr: "a", snapshotGradedAt: time.Time{}},
	}
	got2 := applyPaidProbeBudget(reversed, 2)
	if got2[0].addr != "a" || got2[1].addr != "b" {
		t.Errorf("tie-break must ignore input order, got %s, %s", got2[0].addr, got2[1].addr)
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

// TestGlobalProbeDialLimiter_Constants checks the token-bucket wiring is
// present (rate/burst are sane). It does NOT exercise Wait() timing — a
// real rate-limit assertion needs an injectable clock (the production
// limiter is a process-global at test startup), so this is a wiring check,
// not a throughput check. The behavior itself is covered indirectly by the
// full provider suite (the dial limiter must not break probe timing).
// TestGlobalProbeDialLimiter_DenialIsBoxSide pins the Opus CRITICAL-1 fix
// end-to-end at the helper boundary: when the global bucket is drained, a
// dial must report attempted=false (box-side), NOT a proxy failure. This is
// the regression that previously produced a decidable F on zero dials.
func TestGlobalProbeDialLimiter_DenialIsBoxSide(t *testing.T) {
	// Swap in an EMPTY limiter so the denial is deterministic and the shared
	// bucket is untouched (no pollution for other tests).
	orig := globalProbeDialLimiter
	globalProbeDialLimiter = rate.NewLimiter(rate.Limit(maxProbeDialsPerSec), 0)
	t.Cleanup(func() { globalProbeDialLimiter = orig })

	answered, attempted := probeSocks5Connect(context.Background(), "127.0.0.1:1", "", "", nil, 443, 20*time.Millisecond)
	if attempted {
		t.Errorf("limiter-denied dial must report attempted=false (box-side), got attempted=true")
	}
	if answered {
		t.Error("limiter-denied dial must not report answered")
	}
}

// TestGlobalProbeDialLimiter_Constants checks the token-bucket wiring is
// present (rate/burst are sane). It does NOT exercise Wait() timing — see
// TestGlobalProbeDialLimiter_DenialIsBoxSide for the behavioral regression.
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

// ---------- helpers -------------------------------------------------------

// seedOnlyOneProbeHost makes the box resolve exactly ONE host of the base
// block (plus nothing else), so a probe REACHES the proxy (Total>0) but cannot
// reach the decidable quorum (half the sample) — the DNS-gutted box a 'pending'
// status exists to represent. The resolveProbeTarget fail-cache is cleared for
// every base host, but only the first is given a resolution.
func seedOnlyOneProbeHost(t *testing.T, address string) {
	t.Helper()
	cfg := resolveProxyTableProbeConfig()
	// Pin the pass counter for the whole test so a concurrent URL fetch (which
	// advances tableProbePassCounter) cannot move the probe's internal seed
	// between the block seeded below (base + growth widths) and the probe read
	// inside runPaidProxyGradeOnce — an unseeded block hits real DNS / the
	// fail-cache and the Pending/LastGraded asserts flake (same class as the
	// ViabilityAbort pin-fix, CR review MINOR).
	origPass := tableProbePassCounter.Load()
	tableProbePassCounter.Store(origPass)
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
		tableProbePassCounter.Store(origPass)
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range hosts {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})
}

// TestDisjointGrowthHosts_NoBaseOverlap is a FORWARD guard on the disjoint
// growth invariant (Sonnet MEDIUM B): the same-width stride-tiling in
// disjointGrowthHosts must keep the growth block free of ANY base-block host
// and internally distinct, across many (address, pass) combos. It does not
// invoke the superseded different-width call (that path no longer exists), so
// it is a regression guard on the design guarantee, not a replay of the old
// collision — the invariant it enforces is the one the fix exists to hold.
func TestDisjointGrowthHosts_NoBaseOverlap(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MaxSampleWidth = 12
	extra := cfg.MaxSampleWidth - cfg.SampleWidth
	for p := 0; p < 400; p++ {
		seed := tableProbeSeed("1.2.3.4:1080", uint64(p))
		base, _ := connect.SampleProbeTargets(seed, cfg.SampleWidth)
		baseSet := map[string]bool{}
		for _, h := range base {
			baseSet[h] = true
		}
		grown := disjointGrowthHosts("1.2.3.4:1080", uint64(p), cfg.SampleWidth, extra)
		if len(grown) != extra {
			t.Errorf("pass %d: growth returned %d hosts, want %d", p, len(grown), extra)
			return
		}
		seen := map[string]bool{}
		for _, h := range grown {
			if baseSet[h] {
				t.Errorf("pass %d: growth host %q collides with base", p, h)
			}
			if seen[h] {
				t.Errorf("pass %d: duplicate host %q inside growth block", p, h)
			}
			seen[h] = true
		}
	}
}

// --- stage-0 liveness gate (paid-only) -------------------------------------

// TestPaidGrader_Stage0DropsDeadProxyInOneDial: with Stage0Liveness enabled, a
// paid proxy that cannot even complete a SOCKS5 greeting (dead port) is dropped
// after the single stage-0 dial — no table sample is wasted on it, and no
// grade/pending is recorded (it is simply not evaluated this pass).
func TestPaidGrader_Stage0DropsDeadProxyInOneDial(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, true)
	// A DEAD proxy address: nothing listening on the port (stage-0 greeting
	// fails on dial, so the sweep returns before the table probe).
	addr := closedPortAddr(t)

	src := filepathJoinHome(t, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 5, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	// Dead-at-stage-0 => no grade, no pending. LastGraded DOES advance (it is
	// set unconditionally for any target reaching the apply loop), which is
	// correct: it paces re-probing to the 3-6h paid cadence so a transient
	// backend outage self-heals and is never re-dialed every tick.
	if e.Graded {
		t.Errorf("dead-at-stage-0 proxy must not be graded: %+v", e)
	}
	if e.Pending {
		t.Errorf("dead-at-stage-0 proxy must not be pending: %+v", e)
	}
}

// --- start-at-6 adaptive growth --------------------------------------------

// TestProbeTableThroughProxy_StartAtSixGrowsForBorderline: with MinSampleWidth
// staging (base 6) and MaxSampleWidth 36, a proxy whose base-6 score is
// borderline (within Band of PassBar) GROWS toward the larger width, so the
// uncertain middle gets the wider sample (start-small, expand while borderline).
func TestProbeTableThroughProxy_StartAtSixGrowsForBorderline(t *testing.T) {
	// Pattern: fail the 1st and 6th of a 6-wide base -> base score 4/6=0.667,
	// inside [0.45,0.75], so it must grow past width 6.
	repFor := func(n int) byte {
		if n == 1 || n == 6 {
			return 0x05
		}
		return 0x00
	}
	addr, connects, cleanup := listenSocks5Sequenced(t, repFor)
	defer cleanup()
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 36
	cfg.PassBar = 0.6
	cfg.BorderlineBand = 0.15
	cfg.TargetTimeout = time.Second
	pass := tableProbePassCounter.Load()
	seedProbeDNSForBlocks(t, addr, cfg, pass)

	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	after := connects.Load()
	if res.SampleWidth <= cfg.MinSampleWidth {
		t.Errorf("borderline-at-6 proxy must grow past %d: SampleWidth=%d", cfg.MinSampleWidth, res.SampleWidth)
	}
	if !res.Decidable || res.Total <= 0 {
		t.Errorf("grown pass must be decidable+attempted: decidable=%v total=%d", res.Decidable, res.Total)
	}
	// Dial count must equal the grown intended width (all resolvable).
	if int(after-before) != res.SampleWidth {
		t.Errorf("dial count %d should equal grown SampleWidth %d", after-before, res.SampleWidth)
	}
}

// TestProbeTableThroughProxy_StartAtSixNoGrowForClearlyGood: a clearly-good
// proxy settles at the SMALL base (6) and does NOT pay for the wider sample.
func TestProbeTableThroughProxy_StartAtSixNoGrowForClearlyGood(t *testing.T) {
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 36
	cfg.TargetTimeout = time.Second
	pass := tableProbePassCounter.Load()
	seedProbeDNSForBlocks(t, addr, cfg, pass)

	before := connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	after := connects.Load()
	if res.SampleWidth != 6 {
		t.Errorf("clearly-good proxy must NOT grow (settle at 6): SampleWidth=%d", res.SampleWidth)
	}
	if res.Score != 1.0 {
		t.Errorf("expected score 1.0, got %v", res.Score)
	}
	if int(after-before) != 6 {
		t.Errorf("expected 6 dials (start-at-6, no grow), got %d", after-before)
	}
}
