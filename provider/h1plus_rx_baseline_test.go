package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The two receive counters are exported beside the write ones.
func TestH1PlusMetricsReportReceiveCounters(t *testing.T) {
	var b strings.Builder
	writeH1PlusMetricsStats(&b, connect.H1PlusStatsSnapshot{RxMessages: 7, RxBytes: 7000})
	text := b.String()
	for _, want := range []string{
		"# TYPE urnet_h1plus_rx_messages_total counter\nurnet_h1plus_rx_messages_total 7\n",
		"# TYPE urnet_h1plus_rx_bytes_total counter\nurnet_h1plus_rx_bytes_total 7000\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, text)
		}
	}
}

// The baseline splits H1 by carrier once H1+ has carried something, and keeps
// its old shape on a box that only ran plain websockets.
func TestBaselineTransportCarrierSplit(t *testing.T) {
	plain := buildBaselineTransport(connect.TransportModeStatsSnapshot{
		H1FramesTx: 3, H1BytesTx: 300,
		H1WebSocketFramesTx: 3, H1WebSocketBytesTx: 300,
	}, connect.H3DatagramSnapshot{})
	if plain.H1WebSocket != nil || plain.H1Plus != nil {
		t.Fatalf("carrier split written without H1+ traffic: %+v / %+v", plain.H1WebSocket, plain.H1Plus)
	}

	modes := connect.TransportModeStatsSnapshot{
		H1FramesTx: 5, H1FramesRx: 6, H1BytesTx: 500, H1BytesRx: 600,
		H1WebSocketFramesTx: 3, H1WebSocketFramesRx: 4, H1WebSocketBytesTx: 300, H1WebSocketBytesRx: 400,
		H1PlusFramesTx: 2, H1PlusFramesRx: 2, H1PlusBytesTx: 200, H1PlusBytesRx: 200,
	}
	split := buildBaselineTransport(modes, connect.H3DatagramSnapshot{})
	if split.H1WebSocket == nil || split.H1Plus == nil {
		t.Fatalf("carrier split missing: %+v / %+v", split.H1WebSocket, split.H1Plus)
	}
	if split.H1WebSocket.BytesTx+split.H1Plus.BytesTx != split.H1.BytesTx ||
		split.H1WebSocket.BytesRx+split.H1Plus.BytesRx != split.H1.BytesRx {
		t.Fatalf("carriers do not sum to the h1 total: %+v %+v %+v", split.H1, split.H1WebSocket, split.H1Plus)
	}
}
