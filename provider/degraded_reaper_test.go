package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestDegradedReaperKeepCount(t *testing.T) {
	tests := []struct {
		total int
		want  int
	}{
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 2},
		{4, 2},
		{5, 3},
		{10, 5},
		{49, 25},
		{50, 25},
		{51, 26},
		{100, 50},
		{199, 100},
		{200, 100},
		{4001, 2001},
	}
	for _, tt := range tests {
		got := degradedReaperKeepCount(tt.total)
		if got != tt.want {
			t.Errorf("degradedReaperKeepCount(%d) = %d, want %d", tt.total, got, tt.want)
		}
	}
}

func TestDegradedReaper_ContributionRanking(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "low:1", TotalRxBytes: 100, TotalTxBytes: 50},
		{Index: 1, Address: "high:1", TotalRxBytes: 10000, TotalTxBytes: 5000},
		{Index: 2, Address: "mid:1", TotalRxBytes: 1000, TotalTxBytes: 500},
	}

	scored := scoreDegradedProxies(entries, nil)
	if len(scored) != 3 {
		t.Fatalf("expected 3 scored entries, got %d", len(scored))
	}

	if scored[0].entry.Address != "low:1" {
		t.Fatalf("worst contributor should be first, got %s", scored[0].entry.Address)
	}
	if scored[1].entry.Address != "mid:1" {
		t.Fatalf("mid contributor should be second, got %s", scored[1].entry.Address)
	}
	if scored[2].entry.Address != "high:1" {
		t.Fatalf("best contributor should be last, got %s", scored[2].entry.Address)
	}
}

func TestDegradedReaper_ContractScoreAddsToTraffic(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "traffic_only:1", TotalRxBytes: 1000, TotalTxBytes: 500},
		{Index: 1, Address: "contracts_help:1", TotalRxBytes: 1000, TotalTxBytes: 500},
	}

	contracts := map[int]int64{
		0: 0,
		1: 10,
	}
	getContracts := func(idx int) int64 {
		return contracts[idx]
	}

	scored := scoreDegradedProxies(entries, getContracts)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored entries, got %d", len(scored))
	}

	if scored[0].entry.Address != "traffic_only:1" {
		t.Fatalf("same traffic but no contracts should rank lower, got %s", scored[0].entry.Address)
	}
	if scored[1].entry.Address != "contracts_help:1" {
		t.Fatalf("same traffic with contracts should rank higher, got %s", scored[1].entry.Address)
	}
}

func TestDegradedReaper_ZeroTraffic(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "zero:1"},
		{Index: 1, Address: "some:1", TotalRxBytes: 500},
	}

	scored := scoreDegradedProxies(entries, nil)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored entries, got %d", len(scored))
	}
	if scored[0].entry.Address != "zero:1" {
		t.Fatalf("zero traffic should rank lowest, got %s", scored[0].entry.Address)
	}
}

func TestDegradedReaper_StableSortKeepsEqualScoresStable(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "a:1", TotalRxBytes: 100, TotalTxBytes: 50},
		{Index: 1, Address: "b:1", TotalRxBytes: 100, TotalTxBytes: 50},
	}

	scored := scoreDegradedProxies(entries, nil)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored entries, got %d", len(scored))
	}
	if scored[0].entry.Index != 0 || scored[1].entry.Index != 1 {
		t.Fatal("stable sort should preserve original order for equal scores")
	}
}

func TestDegradedReaper_SelectKeepsBestContributors(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "worst:1", TotalRxBytes: 10, DownFor: degradedReaperMinDownTime},
		{Index: 1, Address: "mid:1", TotalRxBytes: 100, DownFor: degradedReaperMinDownTime},
		{Index: 2, Address: "best:1", TotalRxBytes: 1000, DownFor: degradedReaperMinDownTime},
		{Index: 3, Address: "also_best:1", TotalRxBytes: 900, DownFor: degradedReaperMinDownTime},
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	// With 4 entries and keepPct=50, keep=2, so the worst 2 should be reaped.
	if len(toReap) != 2 {
		t.Fatalf("expected 2 reaped, got %d", len(toReap))
	}
	if toReap[0].Address != "worst:1" {
		t.Fatalf("worst performer should be reaped first, got %s", toReap[0].Address)
	}
	if toReap[1].Address != "mid:1" {
		t.Fatalf("mid performer should be reaped second, got %s", toReap[1].Address)
	}
	for _, addr := range []string{"best:1", "also_best:1"} {
		for _, r := range toReap {
			if r.Address == addr {
				t.Fatalf("best contributor %s should not be reaped", addr)
			}
		}
	}
}

func TestDegradedReaper_RespectsMinDownTime(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "above:1", DownFor: degradedReaperMinDownTime + 1},
		{Index: 1, Address: "below:1", DownFor: 10 * time.Minute},
		{Index: 2, Address: "far_above:1", DownFor: 60 * time.Minute},
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	// With 3 entries and keepPct=50, keep=2 (ceil 1.5), so only scored[0] is
	// even a reap candidate. All three entries have equal (zero) traffic
	// score, so the stable sort keeps them in input order — scored[0] is
	// "above:1", which clears the min-down-time gate and gets reaped.
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	if len(toReap) != 1 || toReap[0].Address != "above:1" {
		t.Fatalf("expected only above:1 to be reaped, got %v", toReap)
	}
}

func TestDegradedReaper_MinDownTimeBoundaryIsInclusive(t *testing.T) {
	// DownFor exactly equal to the threshold must still be reap-eligible —
	// the production check is `DownFor < minDownTime`, so equality passes.
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "exact:1", DownFor: degradedReaperMinDownTime},
		{Index: 1, Address: "one_ns_short:1", DownFor: degradedReaperMinDownTime - 1},
	}

	scored := scoreDegradedProxies(entries, nil)
	// Force both into the reap zone by keeping 0.
	toReap := selectProxiesToReap(scored, 0, degradedReaperMinDownTime)

	if len(toReap) != 1 || toReap[0].Address != "exact:1" {
		t.Fatalf("expected only exact:1 (DownFor == threshold) to be reaped, got %v", toReap)
	}
}

func TestDegradedReaper_AllAboveThreshold(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "a:1", DownFor: 60 * time.Minute},
		{Index: 1, Address: "b:1", DownFor: 90 * time.Minute},
		{Index: 2, Address: "c:1", DownFor: 120 * time.Minute},
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	// With 3 entries: keep=2, only index 0 (in score order) is a candidate.
	// All are ≥30min, so the worst contributor is reaped.
	if len(toReap) != 1 {
		t.Fatalf("expected 1 reaped, got %d", len(toReap))
	}
}

func TestDegradedReaper_EmptyOrSingleDegradedDoesNothing(t *testing.T) {
	cancelMap := map[string]context.CancelFunc{"p:1": func() {}}
	var cancelMu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the goroutine exits after one tick

	done := make(chan struct{})
	go func() {
		runDegradedProxyReaper(ctx, cancelMap, &cancelMu)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper should exit immediately on cancelled context")
	}
}

func TestDegradedReaper_MissingCancelEntry(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "exists:1", TotalRxBytes: 10, DownFor: degradedReaperMinDownTime},
		{Index: 1, Address: "missing:1", TotalRxBytes: 100, DownFor: degradedReaperMinDownTime},
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	cancelMap := map[string]context.CancelFunc{
		"exists:1": func() {},
	}
	var cancelMu sync.Mutex

	reaped := reapProxies(toReap, cancelMap, &cancelMu, alwaysDegraded)

	if reaped != 1 {
		t.Fatalf("expected 1 reaped (missing entry silently skipped), got %d", reaped)
	}
}

func TestDegradedReaper_LargeScale(t *testing.T) {
	n := 4001
	entries := make([]connect.DegradedProxyEntry, n)
	cancelMap := make(map[string]context.CancelFunc, n)
	cancelled := make(map[string]bool, n)
	var callMu sync.Mutex

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("p%d:1", i)
		rx := uint64(rng.Int63n(1 << 40))
		tx := uint64(rng.Int63n(1 << 40))
		entries[i] = connect.DegradedProxyEntry{
			Index:        i,
			Address:      addr,
			DownFor:      degradedReaperMinDownTime + time.Duration(rng.Intn(360))*time.Minute,
			TotalRxBytes: rx,
			TotalTxBytes: tx,
		}
		addrCopy := addr
		cancelMap[addr] = func() {
			callMu.Lock()
			cancelled[addrCopy] = true
			callMu.Unlock()
		}
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	if keep != 2001 {
		t.Fatalf("keep count for 4001: got %d, want 2001", keep)
	}

	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)
	if len(toReap) != 2000 {
		t.Fatalf("expected 2000 reaped (4001-2001), got %d", len(toReap))
	}

	var cancelMu sync.Mutex
	reapedCount := reapProxies(toReap, cancelMap, &cancelMu, alwaysDegraded)
	if reapedCount != 2000 {
		t.Fatalf("expected reapProxies to report 2000 reaped, got %d", reapedCount)
	}

	callMu.Lock()
	total := len(cancelled)
	callMu.Unlock()
	if total != 2000 {
		t.Fatalf("expected 2000 cancel calls, got %d", total)
	}

	// Identity check, not just count: every reaped address must score no
	// higher than every kept address, otherwise a best performer got killed
	// and a worse one survived — the exact regression CodeRabbit flagged.
	maxReapedScore := scored[len(toReap)-1].score
	minKeptScore := scored[len(toReap)].score
	if maxReapedScore > minKeptScore {
		t.Fatalf("a reaped proxy (score %d) outranks a kept one (score %d)", maxReapedScore, minKeptScore)
	}
	for _, p := range toReap {
		if !cancelled[p.Address] {
			t.Fatalf("proxy %s was selected for reaping but never cancelled", p.Address)
		}
	}
}

func TestDegradedReaper_AllDegradedUnder30Min(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "a:1", DownFor: 5 * time.Minute},
		{Index: 1, Address: "b:1", DownFor: 10 * time.Minute},
		{Index: 2, Address: "c:1", DownFor: 20 * time.Minute},
	}

	scored := scoreDegradedProxies(entries, nil)
	keep := degradedReaperKeepCount(len(scored))
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	if len(toReap) != 0 {
		t.Fatalf("expected 0 reaped (all under 30m), got %d", len(toReap))
	}
}

func TestDegradedReaper_LiveContractsAcquiredMissingIndexReturnsZero(t *testing.T) {
	// No metrics have been registered for this index — get() returns nil,
	// and liveContractsAcquired must treat that as "no contracts" rather
	// than panicking on a nil-pointer snapshot() call.
	got := liveContractsAcquired(999999)
	if got != 0 {
		t.Fatalf("liveContractsAcquired for unregistered index = %d, want 0", got)
	}
}

// alwaysDegraded is a stillDegradedFunc stub for tests that exercise
// reapProxies but aren't specifically testing the recheck behavior.
func alwaysDegraded(string) bool { return true }

func TestOnlyCancellableProxies_ExcludesDirect(t *testing.T) {
	// "direct" (native mode) is registered in the same health tracking as any
	// proxy and can appear in connect.DegradedProxies(), but is deliberately
	// never added to proxyCancelMap (must be immune to hot-reload deletions)
	// and must never be reaped.
	degraded := []connect.DegradedProxyEntry{
		{Index: 0, Address: "direct", TotalRxBytes: 999999999}, // best contributor by traffic
		{Index: 1, Address: "proxy1:1", TotalRxBytes: 10},
		{Index: 2, Address: "proxy2:1", TotalRxBytes: 20},
	}
	cancelMap := map[string]context.CancelFunc{
		"proxy1:1": func() {},
		"proxy2:1": func() {},
		// deliberately no entry for "direct"
	}
	var cancelMu sync.Mutex

	cancellable := onlyCancellableProxies(degraded, cancelMap, &cancelMu)

	if len(cancellable) != 2 {
		t.Fatalf("expected 2 cancellable proxies, got %d", len(cancellable))
	}
	for _, p := range cancellable {
		if p.Address == "direct" {
			t.Fatal("direct must never be included in the cancellable set")
		}
	}
}

func TestOnlyCancellableProxies_EmptyWhenNoneCancellable(t *testing.T) {
	degraded := []connect.DegradedProxyEntry{
		{Index: 0, Address: "direct", TotalRxBytes: 10},
	}
	cancelMap := map[string]context.CancelFunc{}
	var cancelMu sync.Mutex

	cancellable := onlyCancellableProxies(degraded, cancelMap, &cancelMu)

	if len(cancellable) != 0 {
		t.Fatalf("expected 0 cancellable proxies, got %d", len(cancellable))
	}
}

func TestDirectNeverReachesReapDecision_EndToEnd(t *testing.T) {
	// Full pipeline check: even though "direct" is the worst-ish contributor
	// by score in a naive read, it must never appear in toReap because
	// onlyCancellableProxies removes it before scoring ever happens.
	degraded := []connect.DegradedProxyEntry{
		{Index: 0, Address: "direct", TotalRxBytes: 0, DownFor: time.Hour}, // worst score, oldest down
		{Index: 1, Address: "proxy1:1", TotalRxBytes: 10, DownFor: time.Hour},
		{Index: 2, Address: "proxy2:1", TotalRxBytes: 20, DownFor: time.Hour},
	}
	cancelMap := map[string]context.CancelFunc{
		"proxy1:1": func() {},
		"proxy2:1": func() {},
	}
	var cancelMu sync.Mutex

	cancellable := onlyCancellableProxies(degraded, cancelMap, &cancelMu)
	scored := scoreDegradedProxies(cancellable, nil)
	keep := degradedReaperKeepCount(len(scored))
	toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

	for _, p := range toReap {
		if p.Address == "direct" {
			t.Fatal("direct must never be selected for reaping")
		}
	}
}

func TestReapProxies_SkipsProxyThatRecoveredSinceDecision(t *testing.T) {
	toReap := []connect.DegradedProxyEntry{
		{Index: 0, Address: "recovered:1"},
		{Index: 1, Address: "still_down:1"},
	}
	var cancelled []string
	cancelMap := map[string]context.CancelFunc{
		"recovered:1":  func() { cancelled = append(cancelled, "recovered:1") },
		"still_down:1": func() { cancelled = append(cancelled, "still_down:1") },
	}
	var cancelMu sync.Mutex

	// Simulate "recovered:1" having reconnected in the gap between the
	// scoring pass and this cancel pass — exactly the race CodeRabbit
	// flagged. isStillDegraded must be re-checked right before cancelling,
	// not assumed from the (by-then-stale) toReap decision.
	isStillDegraded := func(addr string) bool {
		return addr != "recovered:1"
	}

	reaped := reapProxies(toReap, cancelMap, &cancelMu, isStillDegraded)

	if reaped != 1 {
		t.Fatalf("expected 1 reaped (recovered proxy skipped), got %d", reaped)
	}
	if len(cancelled) != 1 || cancelled[0] != "still_down:1" {
		t.Fatalf("expected only still_down:1 to be cancelled, got %v", cancelled)
	}
	if _, ok := cancelMap["recovered:1"]; !ok {
		t.Fatal("recovered:1's cancel map entry must not be deleted — it was never actually cancelled")
	}
	if _, ok := cancelMap["still_down:1"]; ok {
		t.Fatal("still_down:1's cancel map entry should have been deleted after cancelling")
	}
}

func TestReapProxies_CancelsAndDeletesWhenStillDegraded(t *testing.T) {
	toReap := []connect.DegradedProxyEntry{
		{Index: 0, Address: "stuck:1"},
	}
	called := false
	cancelMap := map[string]context.CancelFunc{
		"stuck:1": func() { called = true },
	}
	var cancelMu sync.Mutex

	reaped := reapProxies(toReap, cancelMap, &cancelMu, alwaysDegraded)

	if reaped != 1 {
		t.Fatalf("expected 1 reaped, got %d", reaped)
	}
	if !called {
		t.Fatal("expected cancel to be called")
	}
	if _, ok := cancelMap["stuck:1"]; ok {
		t.Fatal("expected cancel map entry to be deleted after reaping")
	}
}
