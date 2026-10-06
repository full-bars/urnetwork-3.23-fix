package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// End-to-end and floor-boundary tests for the spread sampler + confirmation
// floor. These drive the REAL verdict pipeline (probeTableThroughProxy) rather
// than asserting on block composition, so they pin the outcome the change exists
// for, not the mechanism.

// listenSocks5DenyIPs answers CONNECT with REP 0x02 for any destination IP in
// deny and REP 0x00 otherwise. Destination-keyed, so it models a gateway whose
// ruleset refuses a set of destinations — which is the whole point: the proxy is
// alive and answering, it simply will not serve those destinations.
func listenSocks5DenyIPs(t *testing.T, deny map[string]bool) (addr string, cleanup func()) {
	t.Helper()
	addr, cleanup = listenSocks5Raw(t, func(c net.Conn) {
		defer c.Close()
		if !readSocks5Greeting(c) {
			return
		}
		_, _ = c.Write([]byte{0x05, 0x00})
		frame := make([]byte, 10)
		if _, err := io.ReadFull(c, frame); err != nil {
			return
		}
		rep := byte(0x00)
		if frame[3] == 0x01 { // ATYP IPv4: the probe dials the resolved address
			if deny[net.IPv4(frame[4], frame[5], frame[6], frame[7]).String()] {
				rep = 0x02
			}
		}
		_, _ = c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	})
	return addr, cleanup
}

// seedTableAllHosts points every table row at a destination the fake gateway
// serves, except the named denied rows which get distinct refused addresses.
// Literal-IP rows resolve to themselves (resolveProbeTarget parses them before
// the cache), which is fine: the fake answers on the destination, not by dialing.
func seedTableAllHosts(t *testing.T, denied map[string]string) {
	t.Helper()
	const allowed = "198.51.100.1"
	hosts := connect.ProbeHostNames()
	probeDNSCache.Lock()
	for _, h := range hosts {
		ip := allowed
		if d, ok := denied[h]; ok {
			ip = d
		}
		probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP(ip), at: time.Now()}
		delete(probeDNSCache.fail, h)
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

// TestProbeVerdictPipeline_RegionalDenyDoesNotBlackOutTheSpreadSampler is the
// end-to-end regression. A gateway refuses one authored contiguous region; the
// probe runs many consecutive passes through the real pipeline. The legacy
// sampler can land a whole block inside that region and seal a decidable 0.0
// conviction; the spread sampler must never black out a pass entirely.
//
// Falsifiable: revert the sampler to contiguous blocks and the spread case
// starts producing 0.0 convictions again.
func TestProbeVerdictPipeline_RegionalDenyDoesNotBlackOutTheSpreadSampler(t *testing.T) {
	withTempHome(t)

	// a contiguous run of authored rows — the shape the fleet's recorded
	// failures had (runs of adjacent indices)
	table := connect.ProbeHostNames()
	// indices 104..115: a contiguous authored run (the KR/JP/IN tail sits at
	// 107-109) — the table is 127 rows, so stay inside it
	region := table[104:116]
	denied := map[string]string{}
	deny := map[string]bool{}
	for i, h := range region {
		ip := net.IPv4(203, 0, 113, byte(i+1)).String()
		denied[h] = ip
		deny[ip] = true
	}
	addr, cleanup := listenSocks5DenyIPs(t, deny)
	defer cleanup()
	seedTableAllHosts(t, denied)

	origPass := tableProbePassCounter.Load()
	t.Cleanup(func() { tableProbePassCounter.Store(origPass) })

	baseCfg := func(spread bool) proxyTableProbeConfig {
		cfg := defaultProxyTableProbeConfig()
		cfg.SampleWidth = 6
		cfg.MinSampleWidth = 6
		cfg.MaxSampleWidth = 6 // no growth: isolate the base verdict
		cfg.UseSpreadOrder = spread
		cfg.MinConfirmDials = 0
		cfg.TargetTimeout = 300 * time.Millisecond
		return cfg
	}

	// Deterministic precondition: find the pass values whose CONTIGUOUS block
	// lands entirely inside the denied region. Computing them (rather than
	// sweeping and hoping) keeps the test exact — no flake, and only a handful
	// of passes actually need to run through the pipeline.
	const regionStart, regionEnd = 104, 116
	blocked := []uint64{}
	for pass := uint64(0); pass < 127; pass++ {
		start := int((tableProbeSeed(addr, pass) * 6) % 127)
		if start >= regionStart && start+6 <= regionEnd {
			blocked = append(blocked, pass)
		}
	}
	if len(blocked) == 0 {
		t.Fatalf("fixture is wrong: no pass lands a contiguous 6-wide block inside rows %d..%d",
			regionStart, regionEnd)
	}

	// every one of those passes MUST black out under the contiguous sampler
	for _, pass := range blocked {
		tableProbePassCounter.Store(pass)
		res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, baseCfg(false))
		if !res.Decidable || res.Score != 0 {
			t.Errorf("contiguous pass %d landed wholly in the denied region but did not black out: "+
				"decidable=%v score=%v total=%d", pass, res.Decidable, res.Score, res.Total)
		}
	}

	// the spread sampler must never black out a pass, over a broad sweep
	const sweep = 30
	for pass := uint64(0); pass < sweep; pass++ {
		tableProbePassCounter.Store(pass)
		res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, baseCfg(true))
		if res.Decidable && res.Score == 0 {
			t.Errorf("spread pass %d blacked out; a %d-row denied region must not be able to occupy "+
				"a whole 6-wide spread block", pass, len(region))
		}
	}
}

// TestMinConfirmDials_GrowthLoopAlsoRespectsTheFloor: the floor must bound the
// GROWTH loop's abort too, not only the base loop's. When the base cannot reach
// the floor (part of its block is unresolvable), the growth pass must carry the
// attempt count up to the floor before it may convict.
//
// Falsifiable: drop the floor condition from the growth-loop abort and the pass
// settles below the floor.
func TestMinConfirmDials_GrowthLoopAlsoRespectsTheFloor(t *testing.T) {
	withTempHome(t)
	addr, _, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 12
	cfg.UseSpreadOrder = true
	cfg.MinConfirmDials = 6
	cfg.TargetTimeout = 300 * time.Millisecond

	// make half the base block unresolvable so the base cannot reach the floor
	base := sampleProbeHosts(tableProbeSeed(addr, tableProbePassCounter.Load()), cfg.MinSampleWidth, true)
	probeDNSCache.Lock()
	for i, h := range base {
		if i%2 == 0 {
			delete(probeDNSCache.m, h)
			probeDNSCache.fail[h] = time.Now()
		} else {
			probeDNSCache.m[h] = probeDNSCachedIP{ip: net.ParseIP("198.51.100.1"), at: time.Now()}
			delete(probeDNSCache.fail, h)
		}
	}
	probeDNSCache.Unlock()
	// seed the growth block too, or the growth dials would be unresolvable
	for _, h := range disjointGrowthHosts(addr, tableProbePassCounter.Load(), cfg.MinSampleWidth,
		cfg.MaxSampleWidth-cfg.MinSampleWidth, true) {
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

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if res.Total < cfg.MinConfirmDials {
		t.Errorf("the growth loop convicted below the floor: Total=%d, floor=%d (SampleWidth=%d)",
			res.Total, cfg.MinConfirmDials, res.SampleWidth)
	}
	if !res.Decidable || res.Score != 0 {
		t.Errorf("expected a decidable 0.0 conviction, got decidable=%v score=%v", res.Decidable, res.Score)
	}
}

// TestProbeConfigOverrides_RoundTrip pins the two new JSON keys through the real
// loader, including the negative-floor case that must fall back to the default.
func TestProbeConfigOverrides_RoundTrip(t *testing.T) {
	withTempHome(t)
	cases := []struct {
		name       string
		over       map[string]any
		wantSpread bool
		wantFloor  int
	}{
		{"defaults", map[string]any{}, true, 0},
		{"spread off", map[string]any{"use_spread_order": false}, false, 0},
		{"floor set", map[string]any{"min_confirm_dials": 9}, true, 9},
		{"negative floor ignored", map[string]any{"min_confirm_dials": -1}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeReviewProbeOverride(t, c.over)
			cfg := loadProxyTableProbeConfig()
			if cfg.UseSpreadOrder != c.wantSpread {
				t.Errorf("UseSpreadOrder = %v, want %v", cfg.UseSpreadOrder, c.wantSpread)
			}
			if cfg.MinConfirmDials != c.wantFloor {
				t.Errorf("MinConfirmDials = %d, want %d", cfg.MinConfirmDials, c.wantFloor)
			}
		})
	}
}
