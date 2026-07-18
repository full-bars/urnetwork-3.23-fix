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

	scored := scoreDegradedEntries(entries, nil)
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

	scored := scoreDegradedEntries(entries, getContracts)
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

	scored := scoreDegradedEntries(entries, nil)
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

	scored := scoreDegradedEntries(entries, nil)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored entries, got %d", len(scored))
	}
	if scored[0].entry.Index != 0 || scored[1].entry.Index != 1 {
		t.Fatal("stable sort should preserve original order for equal scores")
	}
}

func TestDegradedReaper_SelectKeepsBestContributors(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "worst:1", TotalRxBytes: 10},
		{Index: 1, Address: "mid:1", TotalRxBytes: 100},
		{Index: 2, Address: "best:1", TotalRxBytes: 1000},
		{Index: 3, Address: "also_best:1", TotalRxBytes: 900},
	}

	type cancelRecorder struct {
		mu   sync.Mutex
		ids  []string
	}
	rec := &cancelRecorder{}
	cancelMap := map[string]context.CancelFunc{
		"worst:1":    func() { rec.mu.Lock(); rec.ids = append(rec.ids, "worst:1"); rec.mu.Unlock() },
		"mid:1":      func() { rec.mu.Lock(); rec.ids = append(rec.ids, "mid:1"); rec.mu.Unlock() },
		"best:1":     func() { rec.mu.Lock(); rec.ids = append(rec.ids, "best:1"); rec.mu.Unlock() },
		"also_best:1": func() { rec.mu.Lock(); rec.ids = append(rec.ids, "also_best:1"); rec.mu.Unlock() },
	}
	var cancelMu sync.Mutex

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	var reaped []string
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		cancelMu.Lock()
		cancel, ok := cancelMap[p.Address]
		if ok {
			cancel()
			reaped = append(reaped, p.Address)
		}
		cancelMu.Unlock()
	}

	// With 4 entries and keepPct=50, keep=2, so worst 2 should be reaped
	if len(reaped) != 2 {
		t.Fatalf("expected 2 reaped, got %d: %v", len(reaped), reaped)
	}

	rec.mu.Lock()
	cancelledIds := rec.ids
	rec.mu.Unlock()

	if len(cancelledIds) != 2 {
		t.Fatalf("expected 2 cancel calls, got %d", len(cancelledIds))
	}
	if cancelledIds[0] != "worst:1" {
		t.Fatalf("worst performer should be cancelled first, got %s", cancelledIds[0])
	}
	if cancelledIds[1] != "mid:1" {
		t.Fatalf("mid performer should be cancelled second, got %s", cancelledIds[1])
	}
}

func TestDegradedReaper_RespectsMinDownTime(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "above:1", DownFor: degradedReaperMinDownTime + 1},
		{Index: 1, Address: "below:1", DownFor: 10 * time.Minute},
		{Index: 2, Address: "far_above:1", DownFor: 60 * time.Minute},
	}

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	// With 3 entries and keepPct=50, keep=2 (ceil 1.5)
	// Process i=0 only (len-keep = 1)
	// scored[0] has the worst (smallest) score — all same score, stable sort
	// so it's "above:1" with DownFor=30min+1 → reaped

	var reaped string
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		if p.DownFor < degradedReaperMinDownTime {
			continue
		}
		reaped = p.Address
	}

	if reaped != "above:1" {
		t.Fatalf("expected above:1 to be reaped (above 30min threshold), got %s", reaped)
	}
}

func TestDegradedReaper_AllAboveThreshold(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "a:1", DownFor: 60 * time.Minute},
		{Index: 1, Address: "b:1", DownFor: 90 * time.Minute},
		{Index: 2, Address: "c:1", DownFor: 120 * time.Minute},
	}

	cancelMap := map[string]context.CancelFunc{}
	for _, e := range entries {
		cancelMap[e.Address] = func() {}
	}
	var cancelMu sync.Mutex

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	var reaped int
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		if p.DownFor < degradedReaperMinDownTime {
			continue
		}
		cancelMu.Lock()
		if cancel, ok := cancelMap[p.Address]; ok {
			cancel()
			reaped++
		}
		cancelMu.Unlock()
	}

	// With 3 entries: keep=2, process index 0 only
	// All ≥30min, so the worst contributor (index 0) is reaped
	if reaped != 1 {
		t.Fatalf("expected 1 reaped, got %d", reaped)
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
		{Index: 0, Address: "exists:1", TotalRxBytes: 10},
		{Index: 1, Address: "missing:1", TotalRxBytes: 100},
	}

	cancelMap := map[string]context.CancelFunc{
		"exists:1": func() {},
	}
	var cancelMu sync.Mutex

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	var reaped int
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		cancelMu.Lock()
		if cancel, ok := cancelMap[p.Address]; ok {
			cancel()
			reaped++
		}
		cancelMu.Unlock()
	}

	if reaped != 1 {
		t.Fatalf("expected 1 reaped (missing entry silently skipped), got %d", reaped)
	}
}

func TestDegradedReaper_LargeScale(t *testing.T) {
	n := 4001
	entries := make([]connect.DegradedProxyEntry, n)
	cancelMap := make(map[string]context.CancelFunc, n)
	callCount := 0
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
		cancelMap[addr] = func() {
			callMu.Lock()
			callCount++
			callMu.Unlock()
		}
	}
	var cancelMu sync.Mutex

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	if keep != 2001 {
		t.Fatalf("keep count for 4001: got %d, want 2001", keep)
	}

	for i := keep; i < len(scored); i++ {
		p := scored[i].entry
		if p.DownFor < degradedReaperMinDownTime {
			continue
		}
		cancelMu.Lock()
		if cancel, ok := cancelMap[p.Address]; ok {
			cancel()
		}
		cancelMu.Unlock()
	}

	callMu.Lock()
	total := callCount
	callMu.Unlock()

	if total != 2000 {
		t.Fatalf("expected 2000 reaped (4001-2001+1), got %d", total)
	}
}

func TestDegradedReaper_AllDegradedUnder30Min(t *testing.T) {
	entries := []connect.DegradedProxyEntry{
		{Index: 0, Address: "a:1", DownFor: 5 * time.Minute},
		{Index: 1, Address: "b:1", DownFor: 10 * time.Minute},
		{Index: 2, Address: "c:1", DownFor: 20 * time.Minute},
	}

	cancelMap := map[string]context.CancelFunc{}
	for _, e := range entries {
		cancelMap[e.Address] = func() {}
	}
	var cancelMu sync.Mutex

	scored := scoreDegradedEntries(entries, nil)
	keep := degradedReaperKeepCount(len(scored))

	var reaped int
	for i := keep; i < len(scored); i++ {
		p := scored[i].entry
		if p.DownFor < degradedReaperMinDownTime {
			continue
		}
		cancelMu.Lock()
		if cancel, ok := cancelMap[p.Address]; ok {
			cancel()
			reaped++
		}
		cancelMu.Unlock()
	}

	if reaped != 0 {
		t.Fatalf("expected 0 reaped (all under 30m), got %d", reaped)
	}
}

// scoreDegradedEntries is the scoring helper extracted from runDegradedProxyReaper
// for testability. getContracts returns contracts acquired for a proxy index, or nil.
func scoreDegradedEntries(entries []connect.DegradedProxyEntry, getContracts func(int) int64) []struct {
	entry connect.DegradedProxyEntry
	score uint64
} {
	scored := make([]struct {
		entry connect.DegradedProxyEntry
		score uint64
	}, len(entries))
	for i, d := range entries {
		score := d.TotalRxBytes + d.TotalTxBytes
		if getContracts != nil {
			if acquired := getContracts(d.Index); acquired > 0 {
				score += uint64(acquired) * 1024
			}
		}
		scored[i] = struct {
			entry connect.DegradedProxyEntry
			score uint64
		}{entry: d, score: score}
	}

	sortScoredByScore(scored)
	return scored
}

func sortScoredByScore(scored []struct {
	entry connect.DegradedProxyEntry
	score uint64
}) {
	// stable sort so equal scores preserve input order
	for i := 0; i < len(scored); i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score < scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}
}
