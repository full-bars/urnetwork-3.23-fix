package main

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"
)

// The transports block splits H1 into its two carriers and carries the live
// gauge beside the cumulative counters.
func TestLiveTransportsSplitsH1Carriers(t *testing.T) {
	modes := connect.TransportModeStatsSnapshot{
		H1WebSocketFramesTx: 5, H1WebSocketFramesRx: 6,
		H1WebSocketBytesTx: 500, H1WebSocketBytesRx: 600,
		H1PlusFramesTx: 7, H1PlusFramesRx: 8,
		H1PlusBytesTx: 700, H1PlusBytesRx: 800,
	}
	conns := connect.H1ConnectionStatsSnapshot{WebSocketConnectionCount: 2, H1PlusConnectionCount: 3}
	tr := liveTransportsSample(modes, conns)
	if tr.H1WebSocket == nil || tr.H1Plus == nil {
		t.Fatalf("h1 carriers missing: %+v", tr)
	}
	ws, plus := tr.H1WebSocket, tr.H1Plus
	if ws.BytesTx != 500 || ws.BytesRx != 600 || ws.FramesTx != 5 || ws.FramesRx != 6 || ws.Up != 2 {
		t.Fatalf("websocket = %+v", ws)
	}
	if plus.BytesTx != 700 || plus.BytesRx != 800 || plus.FramesTx != 7 || plus.FramesRx != 8 || plus.Up != 3 {
		t.Fatalf("h1+ = %+v", plus)
	}
	// The two carriers are the whole of H1, so their sum is the H1 total.
	if ws.BytesTx+plus.BytesTx != 1200 || ws.BytesRx+plus.BytesRx != 1400 {
		t.Fatalf("carriers do not sum to the h1 total")
	}
}

// A provider with H1+ off reports a zero h1_plus rather than hiding the kind,
// so the bar always has both H1 segments.
func TestLiveTransportsKeepsH1PlusWhenIdle(t *testing.T) {
	tr := liveTransportsSample(connect.TransportModeStatsSnapshot{H1WebSocketBytesTx: 10}, connect.H1ConnectionStatsSnapshot{})
	if tr.H1Plus == nil {
		t.Fatal("h1_plus absent with H1+ off")
	}
	if tr.H1Plus.BytesTx != 0 || tr.H1Plus.Up != 0 {
		t.Fatalf("h1_plus not zero: %+v", tr.H1Plus)
	}
}

// Modes the provider never ran are left out instead of sent as zeros.
func TestLiveTransportsOmitsUnusedModes(t *testing.T) {
	tr := liveTransportsSample(connect.TransportModeStatsSnapshot{}, connect.H1ConnectionStatsSnapshot{})
	if tr.H3 != nil || tr.DNS != nil || tr.DNSPump != nil {
		t.Fatalf("unused modes reported: h3=%+v dns=%+v pump=%+v", tr.H3, tr.DNS, tr.DNSPump)
	}

	modes := connect.TransportModeStatsSnapshot{
		H3FramesTx: 1, H3BytesTx: 100, H3Up: 1,
		Dns:     connect.PtModeSnapshot{FramesRx: 2, BytesRx: 200, Up: 1},
		DnsPump: connect.PtModeSnapshot{Drops: 1},
	}
	tr = liveTransportsSample(modes, connect.H1ConnectionStatsSnapshot{})
	if tr.H3 == nil || tr.H3.BytesTx != 100 || tr.H3.Up != 1 {
		t.Fatalf("h3 = %+v", tr.H3)
	}
	if tr.DNS == nil || tr.DNS.BytesRx != 200 || tr.DNS.Up != 1 {
		t.Fatalf("dns = %+v", tr.DNS)
	}
	if tr.DNSPump == nil {
		t.Fatal("dns_pump absent after a drop was counted")
	}
	if tr.DNSPump.BytesRx != 0 {
		t.Fatalf("dns_pump counted bytes: %+v", tr.DNSPump)
	}
}

// The traffic reply stays readable by a client that does not know the block.
func TestLiveTrafficTransportsJSONIsAdditive(t *testing.T) {
	sample := liveTrafficSample(snapT0, func() (uint64, uint64, uint64, uint64) { return 1, 2, 3, 4 })
	if sample.Transports == nil {
		t.Fatal("traffic reply carries no transports block")
	}
	body, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		BillableBytes uint64 `json:"billable_bytes"`
		TotalRxBytes  uint64 `json:"total_rx_bytes"`
	}
	if err := json.Unmarshal(body, &old); err != nil {
		t.Fatalf("old reader failed: %v", err)
	}
	if old.BillableBytes != 3 || old.TotalRxBytes != 3 {
		t.Fatalf("old reader read %+v", old)
	}
	var back LiveTraffic
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.Transports == nil || back.Transports.H1WebSocket == nil {
		t.Fatalf("block did not round trip: %+v", back.Transports)
	}
}
