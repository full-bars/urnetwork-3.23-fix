package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// seedEarnTracker marks addr as having earned "now" in the per-address
// tracker (delta-based), which is what the paid grader's earn-skip reads.
func seedEarnTracker(addr string) {
	bw := connect.RegisterProxyBandwidth(9999) // distinct test index
	// First Update establishes the baseline (prevCum = 0, no delta yet).
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	// Second Update advances the counter: a positive delta is now recorded.
	bw.BillableRx.Store(1024 * 1024)
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{addr: bw})
}

// TestPaidProxyGrader_SkipsEarningProxy pins the earn-skip: a paid proxy
// with RECENT billable traffic (delta within paidEarnWindow) must NOT be
// re-probed even when its grade is stale. The grade clock must not advance,
// so a continuously-earning proxy is never probed.
func TestPaidProxyGrader_SkipsEarningProxy(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			// Stale (past the 6h paid window) but WITHIN the 24h
			// force-probe ceiling, so the earn-skip is the deciding
			// factor: earning must suppress the probe here.
			addr: {ID: 1, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-12 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Mark the proxy as having earned recently.
	seedEarnTracker(addr)
	defer connect.UnregisterProxy(9999)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	// Grade must be untouched (no re-probe happened).
	if !e.Graded || e.Score != 0.9 {
		t.Errorf("earning proxy must not be re-graded, got graded=%v score=%v", e.Graded, e.Score)
	}
	// The stale clock must NOT advance: LastGraded stays at -12h.
	if e.LastGraded.After(time.Now().Add(-11 * time.Hour)) {
		t.Errorf("earning proxy stale clock advanced without a probe — earn-skip must leave LastGraded untouched")
	}
	// No CONNECTs through the proxy: the probe never ran.
	if n := connects.Load(); n != 0 {
		t.Fatalf("earning proxy was probed %d times — earn-skip must skip live proxies", n)
	}
}

// TestPaidProxyGrader_ProbesQuietProxy pins the opposite side: a paid proxy
// with NO recent billable traffic and a stale grade MUST still be probed
// (fail-fast on genuine quiet is preserved — only earning proxies are skipped).
func TestPaidProxyGrader_ProbesQuietProxy(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			// Stale (past 6h) but within the 24h ceiling: the only reason
			// to probe is that the proxy is QUIET (never earned).
			addr: {ID: 2, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-12 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// No earn tracker entry: the proxy has never been observed earning.

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// The quiet stale proxy must be probed (12 CONNECTs for the full pass).
	if n := connects.Load(); n == 0 {
		t.Fatal("quiet stale paid proxy was not probed — fail-fast on quiet proxies must be preserved")
	}
}

// TestPaidProxyGrader_ForceProbeCeiling pins the hard ceiling: a proxy that
// HAS been earning recently but whose last REAL probe is older than
// paidForceProbeCeiling must still be probed — "earning" cannot suppress
// the fail-fast path indefinitely (Sonnet findings 2c/4b).
func TestPaidProxyGrader_ForceProbeCeiling(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// LastGraded older than the 24h ceiling: the proxy MUST be probed even
	// though it is currently earning.
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 3, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-paidForceProbeCeiling - time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	seedEarnTracker(addr)
	defer connect.UnregisterProxy(9999)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n == 0 {
		t.Fatal("earning proxy past the force-probe ceiling was not probed — the 24h ceiling must win over earn-skip")
	}
}
