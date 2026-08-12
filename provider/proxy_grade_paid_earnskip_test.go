package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestPaidProxyGrader_SkipsEarningProxy pins the earn-skip: a paid proxy with
// live billable traffic must NOT be re-probed even when its grade is stale —
// the traffic proves it is alive, and probing it spends paid bandwidth to
// learn what the relay already demonstrates. The grade clock must not advance
// either, so a continuously-earning proxy is never probed.
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
			addr: {ID: 1, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-48 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Give the proxy live billable traffic: the earn-skip must see it.
	// Register the address->index mapping so ProxyBandwidthByAddress resolves.
	connect.RegisterProxy(1, addr)
	bw := connect.RegisterProxyBandwidth(1)
	bw.BillableRx.Store(1024 * 1024) // 1 MiB earned
	defer connect.UnregisterProxy(1)
	defer bw.BillableRx.Store(0)
	defer bw.BillableTx.Store(0)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e := state.Proxies[addr]
	// Grade must be untouched (no re-probe happened).
	if !e.Graded || e.Score != 0.9 {
		t.Errorf("earning proxy must not be re-graded, got graded=%v score=%v", e.Graded, e.Score)
	}
	// The stale clock must NOT advance: LastGraded stays at -48h, so the
	// earn-skip is what protects it, not a fresh-grade stamp.
	if e.LastGraded.After(time.Now().Add(-47 * time.Hour)) {
		t.Errorf("earning proxy stale clock advanced without a probe — earn-skip must leave LastGraded untouched")
	}
	// No CONNECTs through the proxy: the probe never ran.
	if n := connects.Load(); n != 0 {
		t.Fatalf("earning proxy was probed %d times — earn-skip must skip live proxies", n)
	}
}

// TestPaidProxyGrader_ProbesQuietProxy pins the opposite side: a paid proxy
// with NO billable traffic and a stale grade MUST still be probed (fail-fast
// on genuine death is preserved — only earning proxies are skipped).
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
			addr: {ID: 2, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-48 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// No billable traffic registered for this proxy.

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// The quiet stale proxy must be probed (12 CONNECTs for the full pass).
	if n := connects.Load(); n == 0 {
		t.Fatal("quiet stale paid proxy was not probed — fail-fast on quiet proxies must be preserved")
	}
}
