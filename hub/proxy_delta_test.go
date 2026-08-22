package main

import (
	"testing"
)

func TestDeltaTracker(t *testing.T) {
	d := newDeltaTracker()
	node := "n1"
	addr := "1.2.3.4:1080"

	// First report: ok=false (no baseline).
	_, ok := d.delta(node, addr, proxyCounters{RX: 100, TX: 50, BillRX: 10, BillTX: 5, Acq: 2, Denied: 1})
	if ok {
		t.Error("first report should return ok=false (no prior baseline)")
	}

	// Second monotonic report: ok=true with correct deltas.
	d2, ok := d.delta(node, addr, proxyCounters{RX: 200, TX: 100, BillRX: 20, BillTX: 10, Acq: 5, Denied: 3})
	if !ok {
		t.Fatal("second monotonic report should return ok=true")
	}
	if d2.RX != 100 || d2.TX != 50 || d2.BillRX != 10 || d2.BillTX != 5 || d2.Acq != 3 || d2.Denied != 2 {
		t.Errorf("deltas = %+v, want RX=100 TX=50 BillRX=10 BillTX=5 Acq=3 Denied=2", d2)
	}

	// Third report: RX drops (provider restart) → ok=false, rebaseline.
	_, ok = d.delta(node, addr, proxyCounters{RX: 50, TX: 30, BillRX: 5, BillTX: 3, Acq: 1, Denied: 0})
	if ok {
		t.Error("counter drop (restart) should return ok=false")
	}

	// Fourth report: monotonic from restart baseline → ok=true.
	d4, ok := d.delta(node, addr, proxyCounters{RX: 150, TX: 80, BillRX: 15, BillTX: 8, Acq: 3, Denied: 2})
	if !ok {
		t.Fatal("fourth monotonic report (post-restart) should return ok=true")
	}
	if d4.RX != 100 || d4.TX != 50 || d4.BillRX != 10 || d4.BillTX != 5 || d4.Acq != 2 || d4.Denied != 2 {
		t.Errorf("post-restart deltas = %+v, want RX=100 TX=50 BillRX=10 BillTX=5 Acq=2 Denied=2", d4)
	}

	// forgetNode: next report should be ok=false again.
	d.forgetNode(node)
	_, ok = d.delta(node, addr, proxyCounters{RX: 200, TX: 100, BillRX: 20, BillTX: 10, Acq: 5, Denied: 3})
	if ok {
		t.Error("after forgetNode, next report should return ok=false (cold start)")
	}

	// Different node should have independent baselines.
	_, ok = d.delta("n2", addr, proxyCounters{RX: 500, TX: 500, BillRX: 50, BillTX: 50, Acq: 10, Denied: 0})
	if ok {
		t.Error("first report for a different node should return ok=false")
	}
	// Forget n1 should NOT affect n2.
	d.forgetNode("n1")
	dn2, ok := d.delta("n2", addr, proxyCounters{RX: 600, TX: 600, BillRX: 60, BillTX: 60, Acq: 15, Denied: 0})
	if !ok {
		t.Fatal("n2 should be unaffected by forgetting n1")
	}
	if dn2.RX != 100 {
		t.Errorf("n2 delta RX = %d, want 100", dn2.RX)
	}
}
