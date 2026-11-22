package connect

import (
	"testing"
)

func wantFamily(t *testing.T, got, want ProxyBandwidthFamily, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %+v, want %+v", what, got, want)
	}
}

// TestProxyBandwidthFamilyTotalsSumsV6 proves ProxyBandwidthFamilyTotals sums
// only the IPv6 counters across every registered proxy.
func TestProxyBandwidthFamilyTotalsSumsV6(t *testing.T) {
	resetProxyHealthForTest()

	if f := ProxyBandwidthFamilyTotals(); f != (ProxyBandwidthFamily{}) {
		t.Fatalf("empty registry = %+v, want all zero", f)
	}

	a := RegisterProxyBandwidth(1)
	a.BillableRx.Store(80)
	a.BillableRxV6.Store(20)
	a.BillableTx.Store(150)
	a.BillableTxV6.Store(10)
	a.TotalRx.Store(100)
	a.TotalRxV6.Store(30)
	a.TotalTx.Store(200)
	a.TotalTxV6.Store(50)

	b := RegisterProxyBandwidth(2)
	b.BillableRxV6.Store(5)
	b.BillableTxV6.Store(6)
	b.TotalRxV6.Store(7)
	b.TotalTxV6.Store(8)

	wantFamily(t, ProxyBandwidthFamilyTotals(), ProxyBandwidthFamily{
		BillableRx: 25, BillableTx: 16,
		TotalRx: 37, TotalTx: 58,
	}, "family totals")

	// The IPv4 share the reader derives is the base total minus this one, so
	// the base must be at least as large: the two are written on the same path.
	brRx, brTx, totRx, totTx := ProxyBandwidthDirectionTotals()
	if brRx < 25 || brTx < 16 || totRx < 37 || totTx < 58 {
		t.Fatalf("base totals %d/%d/%d/%d smaller than the ipv6 share", brRx, brTx, totRx, totTx)
	}
}

// TestBillableRxTxIPv6Split proves the billable accounting paths credit the IPv6
// counters only for IPv6 packets and never swap rx with tx.
func TestBillableRxTxIPv6Split(t *testing.T) {
	bw := &ProxyBandwidth{}
	p := &RemoteUserNatProvider{bw: bw}

	packet4 := []byte{0x45, 0, 0, 0}
	packet6 := []byte{0x60, 0, 0, 0}

	// IPv4 packet: base counters only, no IPv6 add.
	p.countBillableRx(&IpPath{Version: 4}, packet4)
	p.countBillableTx(packet4)
	if bw.BillableRx.Load() != 4 || bw.BillableTx.Load() != 4 ||
		bw.BillableRxV6.Load() != 0 || bw.BillableTxV6.Load() != 0 {
		t.Fatalf("ipv4: rx=%d tx=%d rxV6=%d txV6=%d, want 4/4/0/0",
			bw.BillableRx.Load(), bw.BillableTx.Load(), bw.BillableRxV6.Load(), bw.BillableTxV6.Load())
	}

	// IPv6 packet: base and IPv6 counters both move.
	p.countBillableRx(&IpPath{Version: 6}, packet6)
	p.countBillableTx(packet6)
	if bw.BillableRx.Load() != 8 || bw.BillableTx.Load() != 8 ||
		bw.BillableRxV6.Load() != 4 || bw.BillableTxV6.Load() != 4 {
		t.Fatalf("ipv6: rx=%d tx=%d rxV6=%d txV6=%d, want 8/8/4/4",
			bw.BillableRx.Load(), bw.BillableTx.Load(), bw.BillableRxV6.Load(), bw.BillableTxV6.Load())
	}

	// Mixed stream: v4 and v6 packets interleave, rx and tx not swapped.
	p.countBillableRx(&IpPath{Version: 6}, packet6)
	p.countBillableRx(&IpPath{Version: 4}, packet4)
	p.countBillableTx(packet4)
	p.countBillableTx(packet6)
	if bw.BillableRx.Load() != 16 || bw.BillableRxV6.Load() != 8 {
		t.Fatalf("mixed rx: base=%d v6=%d, want 16/8", bw.BillableRx.Load(), bw.BillableRxV6.Load())
	}
	if bw.BillableTx.Load() != 16 || bw.BillableTxV6.Load() != 8 {
		t.Fatalf("mixed tx: base=%d v6=%d, want 16/8", bw.BillableTx.Load(), bw.BillableTxV6.Load())
	}

	// A nil path falls back to the header nibble.
	p.countBillableRx(nil, packet6)
	if bw.BillableRx.Load() != 20 || bw.BillableRxV6.Load() != 12 {
		t.Fatalf("header fallback: base=%d v6=%d, want 20/12", bw.BillableRx.Load(), bw.BillableRxV6.Load())
	}

	// A nil bw record is a no-op, as the base counters have always been.
	nilBw := &RemoteUserNatProvider{}
	nilBw.countBillableRx(&IpPath{Version: 6}, packet6)
	nilBw.countBillableTx(packet6)
}

// TestProxyBandwidthSnapshotCopiesFamily proves the persistence clone copies the
// IPv6 counters with the rest of the record.
func TestProxyBandwidthSnapshotCopiesFamily(t *testing.T) {
	bw := &ProxyBandwidth{}
	bw.TotalRxV6.Store(11)
	bw.TotalTxV6.Store(22)
	bw.BillableRxV6.Store(33)
	bw.BillableTxV6.Store(44)

	out := proxyBandwidthSnapshot(bw)
	if out.TotalRxV6.Load() != 11 || out.TotalTxV6.Load() != 22 ||
		out.BillableRxV6.Load() != 33 || out.BillableTxV6.Load() != 44 {
		t.Fatalf("snapshot lost the IPv6 counters: %+v", out)
	}
}

// TestSeedProxyBandwidthLeavesV6AtZero proves the seeded record starts its IPv6
// counters at zero: the persisted base totals are lifetime figures that predate
// the IPv6 split, so they read as IPv4 until new IPv6 traffic arrives. The IPv6
// counters are never persisted.
func TestSeedProxyBandwidthLeavesV6AtZero(t *testing.T) {
	resetProxyHealthForTest()

	key := "proxy[0] (10.0.0.1:1080)"
	metricsProxyBase[key] = metricsProxyCounters{
		TotalRx: 1000, TotalTx: 2000, BillableRx: 300, BillableTx: 400,
	}

	bw := &ProxyBandwidth{}
	seedProxyBandwidthLocked(key, bw)
	if bw.TotalRx.Load() != 1000 || bw.TotalTx.Load() != 2000 ||
		bw.BillableRx.Load() != 300 || bw.BillableTx.Load() != 400 {
		t.Fatalf("seeded base counters wrong: %+v", bw)
	}
	if bw.TotalRxV6.Load() != 0 || bw.TotalTxV6.Load() != 0 ||
		bw.BillableRxV6.Load() != 0 || bw.BillableTxV6.Load() != 0 {
		t.Fatal("the seed must not write the IPv6 counters: they are not persisted")
	}
}
