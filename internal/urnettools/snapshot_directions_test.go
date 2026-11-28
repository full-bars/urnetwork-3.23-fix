package urnettools

import (
	"encoding/json"
	"reflect"
	"testing"
)

// An older provider does not send the per-direction fields. They must decode as
// nil so top can tell "no split to draw" (hide the direction pages) from a real
// zero.
func TestLiveTrafficDirectionsAbsentFromOldProvider(t *testing.T) {
	var lt LiveTraffic
	if err := json.Unmarshal([]byte(`{"at_unix_nano":5,"billable_bytes":700,"total_bytes":2100}`), &lt); err != nil {
		t.Fatal(err)
	}
	if lt.BillableRxBytes != nil || lt.BillableTxBytes != nil || lt.TotalRxBytes != nil || lt.TotalTxBytes != nil {
		t.Fatalf("old provider produced direction fields: %+v", lt)
	}
	if lt.BillableBytes != 700 || lt.TotalBytes != 2100 {
		t.Fatalf("sums = %d/%d, want 700/2100", lt.BillableBytes, lt.TotalBytes)
	}
}

// A provider that sends zeros is sending a real split of zero, so the pointers
// stay non-nil even though every value is zero. That is what keeps the pages
// offered on an idle new provider.
func TestLiveTrafficDirectionsPresentAtZero(t *testing.T) {
	var lt LiveTraffic
	body := `{"at_unix_nano":5,"billable_bytes":0,"total_bytes":0,` +
		`"billable_rx_bytes":0,"billable_tx_bytes":0,"total_rx_bytes":0,"total_tx_bytes":0}`
	if err := json.Unmarshal([]byte(body), &lt); err != nil {
		t.Fatal(err)
	}
	if lt.BillableRxBytes == nil || lt.BillableTxBytes == nil || lt.TotalRxBytes == nil || lt.TotalTxBytes == nil {
		t.Fatalf("new provider with zeros produced nil fields: %+v", lt)
	}
	if *lt.BillableRxBytes != 0 || *lt.TotalTxBytes != 0 {
		t.Fatalf("zero split decoded as %+v", lt)
	}
}

// The snapshot block is optional and keeps rx (download) and tx (upload) apart.
func TestSnapshotTrafficDirectionsRoundTrip(t *testing.T) {
	var old SnapshotTraffic
	if err := json.Unmarshal([]byte(`{"billable_bytes":1,"total_bytes":2,"total_history_bps":[3]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Directions != nil {
		t.Fatalf("old provider produced a direction block: %+v", old.Directions)
	}

	body := `{"billable_bytes":9,"total_bytes":14,"directions":{` +
		`"billable_rx":{"now_bps":300,"avg_1m_bps":300,"avg_5m_bps":280,"history_bps":[300,300]},` +
		`"billable_tx":{"now_bps":200,"avg_1m_bps":150,"avg_5m_bps":120,"history_bps":[100,200]},` +
		`"total_rx":{"now_bps":400,"avg_1m_bps":400,"avg_5m_bps":390,"history_bps":[400,400]},` +
		`"total_tx":{"now_bps":300,"avg_1m_bps":250,"avg_5m_bps":210,"history_bps":[200,300]}}}`
	var tr SnapshotTraffic
	if err := json.Unmarshal([]byte(body), &tr); err != nil {
		t.Fatal(err)
	}
	d := tr.Directions
	if d == nil {
		t.Fatal("direction block dropped")
	}
	if !reflect.DeepEqual(d.BillableRx.HistoryBps, []float64{300, 300}) ||
		!reflect.DeepEqual(d.BillableTx.HistoryBps, []float64{100, 200}) ||
		!reflect.DeepEqual(d.TotalRx.HistoryBps, []float64{400, 400}) ||
		!reflect.DeepEqual(d.TotalTx.HistoryBps, []float64{200, 300}) {
		t.Fatalf("history = %+v", d)
	}
	if d.BillableRx.NowBps != 300 || d.BillableRx.Avg5mBps != 280 ||
		d.BillableTx.NowBps != 200 || d.BillableTx.Avg1mBps != 150 ||
		d.TotalRx.NowBps != 400 || d.TotalTx.NowBps != 300 || d.TotalTx.Avg1mBps != 250 {
		t.Fatalf("rates = %+v", d)
	}
}

func TestPacketFieldsCompatibility(t *testing.T) {
	var old SnapshotTraffic
	if err := json.Unmarshal([]byte(`{}`), &old); err != nil || old.Packets != nil {
		t.Fatalf("old snapshot: %+v, %v", old, err)
	}
	var got SnapshotTraffic
	if err := json.Unmarshal([]byte(`{"packets":{"billable_rx":{"history_pps":[3,0]},"billable_tx":{"history_pps":[7,3]}}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Packets == nil || !reflect.DeepEqual(got.Packets.BillableRx.HistoryPps, []float64{3, 0}) || !reflect.DeepEqual(got.Packets.BillableTx.HistoryPps, []float64{7, 3}) {
		t.Fatalf("packets: %+v", got.Packets)
	}
	var live LiveTraffic
	if err := json.Unmarshal([]byte(`{"billable_rx_packets":0,"billable_tx_packets":9}`), &live); err != nil {
		t.Fatal(err)
	}
	if live.BillableRxPackets == nil || *live.BillableRxPackets != 0 || live.BillableTxPackets == nil || *live.BillableTxPackets != 9 {
		t.Fatalf("live: %+v", live)
	}
}

// The IP family split rides inside the direction block. A provider that predates
// the IPv6 counters must decode as no split, so the family page is not offered.
func TestFamilySplitCompatibility(t *testing.T) {
	var old SnapshotTraffic
	if err := json.Unmarshal([]byte(`{"directions":{"billable_rx":{"now_bps":1}}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Directions == nil || old.Directions.Families != nil {
		t.Fatalf("old provider produced a family split: %+v", old.Directions)
	}

	body := `{"directions":{"families":{` +
		`"billable_rx":{"v4":{"now_bps":300,"history_bps":[300,300]},"v6":{"now_bps":100,"history_bps":[100,100]}},` +
		`"billable_tx":{"v4":{"now_bps":200},"v6":{"now_bps":50}},` +
		`"total_rx":{"v4":{"now_bps":400},"v6":{"now_bps":150}},` +
		`"total_tx":{"v4":{"now_bps":300},"v6":{"now_bps":90}}}}}`
	var got SnapshotTraffic
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	fam := got.Directions.Families
	if fam == nil {
		t.Fatal("family split dropped")
	}
	if fam.BillableRx.V4.NowBps != 300 || fam.BillableRx.V6.NowBps != 100 ||
		fam.BillableTx.V6.NowBps != 50 || fam.TotalRx.V6.NowBps != 150 || fam.TotalTx.V6.NowBps != 90 {
		t.Fatalf("family split = %+v", fam)
	}
	if !reflect.DeepEqual(fam.BillableRx.V6.HistoryBps, []float64{100, 100}) {
		t.Fatalf("v6 history = %v", fam.BillableRx.V6.HistoryBps)
	}

	// The live reply carries the IPv6 counters as plain values from a new
	// provider and leaves the pointers nil from an old one.
	var live LiveTraffic
	if err := json.Unmarshal([]byte(`{"billable_rx_bytes_v6":0,"billable_tx_bytes_v6":7,"total_rx_bytes_v6":1,"total_tx_bytes_v6":2}`), &live); err != nil {
		t.Fatal(err)
	}
	if live.BillableRxBytesV6 == nil || *live.BillableRxBytesV6 != 0 || *live.BillableTxBytesV6 != 7 ||
		*live.TotalRxBytesV6 != 1 || *live.TotalTxBytesV6 != 2 {
		t.Fatalf("live ipv6 counters = %+v", live)
	}
	var oldLive LiveTraffic
	if err := json.Unmarshal([]byte(`{"billable_bytes":1}`), &oldLive); err != nil {
		t.Fatal(err)
	}
	if oldLive.BillableRxBytesV6 != nil || oldLive.TotalTxBytesV6 != nil {
		t.Fatalf("old provider produced ipv6 counters: %+v", oldLive)
	}
}
