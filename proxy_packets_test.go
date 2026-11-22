package connect

import "testing"

func TestProxyBandwidthPacketTotals(t *testing.T) {
	resetProxyHealthForTest()
	a := RegisterProxyBandwidth(1)
	b := RegisterProxyBandwidth(2)
	a.BillableRxPackets.Store(10)
	a.BillableTxPackets.Store(3)
	b.BillableRxPackets.Store(2)
	b.BillableTxPackets.Store(7)
	rx, tx := ProxyBandwidthPacketTotals()
	if rx != 12 || tx != 10 {
		t.Fatalf("totals = %d/%d, want 12/10", rx, tx)
	}
	snap := proxyBandwidthSnapshot(a)
	if snap.BillableRxPackets.Load() != 10 || snap.BillableTxPackets.Load() != 3 {
		t.Fatal("snapshot lost packet counters")
	}
	UnregisterProxy(1)
	rx, tx = ProxyBandwidthPacketTotals()
	if rx != 2 || tx != 7 {
		t.Fatalf("after removal = %d/%d, want 2/7", rx, tx)
	}
}
