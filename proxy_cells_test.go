package connect

import (
	"sync"
	"testing"
	"time"
)

func registerSyntheticProxies(t *testing.T, n int) {
	t.Helper()
	ResetProxyHealthForTesting()
	for i := 0; i < n; i++ {
		RegisterProxy(i, "10.0.0.1:1080", "10.0.0.1:1080")
	}
}

func TestProxyCellsStatesAndOrder(t *testing.T) {
	now := time.Now()
	registerSyntheticProxies(t, 6)
	// index 0 up, 1 degraded, 2 connecting (fresh), 3 dead (never up), 4 up,
	// 5 parked.
	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1)
	// leave 2 connecting (fresh registration)
	// index 3: registered but its connecting window expired -> reads dead
	markProxyUp(4)
	markProxyParked(5, true)
	// age the connecting clock of index 3 past connectingStaleAfter
	proxyHealthMu.Lock()
	h3 := proxyHealthByIndex[3]
	h3.connectingSince = now.Add(-connectingStaleAfter - time.Minute)
	proxyHealthMu.Unlock()

	cells := ProxyCells(now)
	if len(cells) != 6 {
		t.Fatalf("got %d cells, want 6", len(cells))
	}
	// Index-sorted and stable.
	for i, c := range cells {
		if c.Index != i {
			t.Fatalf("cells[%d].Index = %d, want %d", i, c.Index, i)
		}
	}
	want := []uint8{ProxyCellUp, ProxyCellDegraded, ProxyCellConnecting, ProxyCellDead, ProxyCellUp, ProxyCellParked}
	for i, c := range cells {
		if c.State != want[i] {
			t.Fatalf("cells[%d].State = %d, want %d", i, c.State, want[i])
		}
	}
	// Address present for every cell.
	for i, c := range cells {
		if c.Addr == "" {
			t.Errorf("cells[%d].Addr empty", i)
		}
	}
}

func TestProxyCellsParkedOwnStateNotConnecting(t *testing.T) {
	ResetProxyHealthForTesting()
	RegisterProxy(0, "10.0.0.1:1080", "10.0.0.1:1080")
	markProxyParked(0, true)
	cells := ProxyCells(time.Now())
	if len(cells) != 1 || cells[0].State != ProxyCellParked {
		t.Fatalf("parked proxy must read ProxyCellParked, got %+v", cells)
	}
}

func TestProxyCellsIndexSortedUnique(t *testing.T) {
	ResetProxyHealthForTesting()
	// Register in descending index order.
	for i := 9; i >= 0; i-- {
		RegisterProxy(i, "10.0.0.1:1080", "10.0.0.1:1080")
	}
	cells := ProxyCells(time.Now())
	if len(cells) != 10 {
		t.Fatalf("got %d cells, want 10", len(cells))
	}
	for i := 1; i < len(cells); i++ {
		if cells[i-1].Index >= cells[i].Index {
			t.Fatalf("cells not strictly index-sorted at %d: %d >= %d", i-1, cells[i-1].Index, cells[i].Index)
		}
	}
}

func TestProxyCellsCheapWith4400Proxies(t *testing.T) {
	const n = 4400
	registerSyntheticProxies(t, n)
	now := time.Now()

	start := time.Now()
	cells := ProxyCells(now)
	elapsed := time.Since(start)

	if len(cells) != n {
		t.Fatalf("got %d cells, want %d", len(cells), n)
	}
	// Index-sorted, unique.
	for i := 1; i < len(cells); i++ {
		if cells[i-1].Index >= cells[i].Index {
			t.Fatalf("cells not strictly index-sorted at %d: %d >= %d", i-1, cells[i-1].Index, cells[i].Index)
		}
	}
	// One lock, one pass: this must be far under the snapshot budget.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("ProxyCells took %s for %d proxies, want well under 50ms", elapsed, n)
	}
	t.Logf("ProxyCells over %d proxies: %s (%d output cells)", n, elapsed, len(cells))
}

func TestProxyCellsAllUpWithClientsAndBytes(t *testing.T) {
	ResetProxyHealthForTesting()
	RegisterProxy(0, "10.0.0.1:1080", "10.0.0.1:1080")
	markProxyUp(0)
	bw := RegisterProxyBandwidth(0)
	bw.TotalRx.Store(1 << 20)
	bw.TotalTx.Store(1 << 20)
	bw.Clients.Store(3)
	cells := ProxyCells(time.Now())
	if len(cells) != 1 {
		t.Fatalf("got %d cells", len(cells))
	}
	c := cells[0]
	if c.State != ProxyCellUp {
		t.Errorf("state = %d, want up", c.State)
	}
	if c.Total != 2<<20 {
		t.Errorf("Total = %d, want %d", c.Total, 2<<20)
	}
	if c.Clients != 3 {
		t.Errorf("Clients = %d, want 3", c.Clients)
	}
}

// TestProxyCellsConcurrentSafe proves the read path never mutates the registry
// (no baseline advance, no counter resets) while a heartbeat runs.
func TestProxyCellsConcurrentSafe(t *testing.T) {
	ResetProxyHealthForTesting()
	registerSyntheticProxies(t, 50)
	now := time.Now()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ProxyHealthHeartbeat(true)
		}
	}()
	for i := 0; i < 200; i++ {
		cells := ProxyCells(now)
		if len(cells) != 50 {
			t.Fatalf("got %d cells, want 50", len(cells))
		}
	}
	close(stop)
	wg.Wait()
}

func BenchmarkProxyCells4400(b *testing.B) {
	ResetProxyHealthForTesting()
	for i := 0; i < 4400; i++ {
		RegisterProxy(i, "10.0.0.1:1080", "10.0.0.1:1080")
	}
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ProxyCells(now)
	}
}
