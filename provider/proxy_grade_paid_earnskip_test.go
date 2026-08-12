package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// earnTrackerTestSeq supplies a distinct health index per seedEarnTracker
// call so tests can never collide on a shared RegisterProxyBandwidth index
// (previously a hardcoded 9999).
var earnTrackerTestSeq atomic.Uint64

// seedEarnTracker marks addr as having earned "now" in the per-address
// tracker (delta-based), which is what the paid grader's earn-skip reads.
// It feeds the tracker the SAME formatted key shape production uses —
// connect.ProxyHealthSnapshot keys its bandwidth map with
// "proxy[N] (addr)" (formatProxyEntry) — so these tests exercise the real
// key format and would catch a regression to raw-address seeding (the
// snapshot-key CRITICAL that made earn-skip dead in production).
func seedEarnTracker(t *testing.T, addr string) {
	t.Helper()
	idx := int(earnTrackerTestSeq.Add(1))
	bw := connect.RegisterProxyBandwidth(idx)
	t.Cleanup(func() { connect.UnregisterProxy(idx) })
	key := fmt.Sprintf("proxy[%d] (%s)", idx, addr)
	// First Update establishes the baseline (prevCum = 0, no delta yet).
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{key: bw})
	// Second Update advances the counter: a positive delta is now recorded.
	bw.BillableRx.Store(1024 * 1024)
	globalPerProxyEarnTracker.Update(map[string]*connect.ProxyBandwidth{key: bw})
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
	seedEarnTracker(t, addr)

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

	seedEarnTracker(t, addr)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n == 0 {
		t.Fatal("earning proxy past the force-probe ceiling was not probed — the 24h ceiling must win over earn-skip")
	}
}

// TestPaidProxyGrader_EarnedTooLongAgoIsProbed pins that earn-skip reads
// the WINDOWED signal (EarnedSince(addr, paidEarnWindow)), not "has this
// address ever earned in this process". A proxy that earned once but not
// within the last paidEarnWindow (15m) is no longer "actively earning" and
// must be probed like any other stale, quiet proxy.
func TestPaidProxyGrader_EarnedTooLongAgoIsProbed(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	// Seed a WINDOW of pass values, not just the current one: this test
	// (unlike the ceiling tests above) actually reaches the probe, since
	// earn-skip does not suppress it, so it needs the same robustness as
	// TestPaidProxyGrader_UndecidableKeepsPriorGrade — see the comment
	// there for why a single Load() is flaky under `go test ./...`.
	base := tableProbePassCounter.Load()
	passes := make([]uint64, 0, 8)
	for i := uint64(0); i < 8; i++ {
		passes = append(passes, base+i)
	}
	seedProbeDNSForAddress(t, addr, passes...)

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			// Stale (past 6h) but within the 24h ceiling.
			addr: {ID: 5, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-12 * time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The proxy earned once, but well outside paidEarnWindow (15m) — it
	// must no longer be treated as "actively earning".
	globalPerProxyEarnTracker.mu.Lock()
	globalPerProxyEarnTracker.lastEarned[addr] = time.Now().Add(-paidEarnWindow - time.Hour)
	globalPerProxyEarnTracker.mu.Unlock()
	t.Cleanup(func() {
		globalPerProxyEarnTracker.mu.Lock()
		delete(globalPerProxyEarnTracker.lastEarned, addr)
		globalPerProxyEarnTracker.mu.Unlock()
	})

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n == 0 {
		t.Fatal("a proxy that earned outside paidEarnWindow must be probed — earn-skip is a RECENCY signal, not a lifetime one")
	}
}

// TestPaidProxyGrader_JustUnderForceProbeCeilingStillSkipped pins the
// other side of the ceiling boundary: an earning proxy whose grade is
// stale but still (barely) under paidForceProbeCeiling must continue to
// be skipped — the ceiling only overrides earn-skip once it is actually
// reached, not preemptively.
func TestPaidProxyGrader_JustUnderForceProbeCeilingStillSkipped(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// One hour shy of the 24h ceiling: still earning must suppress the probe.
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			addr: {ID: 6, Health: "up", Source: "file", Graded: true, Score: 0.9, LastGraded: time.Now().Add(-paidForceProbeCeiling + time.Hour)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	seedEarnTracker(t, addr)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 0 {
		t.Fatalf("earning proxy just under the force-probe ceiling was probed %d times — the ceiling must not fire early", n)
	}
}

// TestPaidProxyGrader_ProbesNeverGradedEarningProxy pins the review
// CRITICAL: a paid proxy with NO grade at all (LastGraded zero) that
// happens to be earning must STILL be probed. Earn-skip must never
// prevent the FIRST grade — the force-probe ceiling is keyed off
// LastGraded, so a never-graded proxy would otherwise be skipped
// forever and stay ungraded indefinitely.
func TestPaidProxyGrader_ProbesNeverGradedEarningProxy(t *testing.T) {
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
			// NEVER graded (LastGraded zero), but actively earning:
			// earn-skip must not suppress the first probe.
			addr: {ID: 4, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Mark the proxy as having earned recently.
	seedEarnTracker(t, addr)

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	// The never-graded earning proxy must be probed.
	if n := connects.Load(); n == 0 {
		t.Fatal("never-graded earning proxy was not probed — the first grade must never be suppressed by earn-skip")
	}
	// And it must now carry a grade.
	state, _ := readProxyState()
	if e, ok := state.Proxies[addr]; !ok || !e.Graded {
		t.Fatalf("never-graded proxy must receive a grade after its first probe, got %+v", e)
	}
}
