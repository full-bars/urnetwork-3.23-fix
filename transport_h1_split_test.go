package connect

import "testing"

// TestH1CarrierCountersSplit proves the payload counters separate the two H1
// carrier kinds and that the H1 totals are exactly their sum, so a reader can
// never see a split that disagrees with the total beside it.
func TestH1CarrierCountersSplit(t *testing.T) {
	var c h1CarrierCounters
	c.counters(false).addTx(100)
	c.counters(false).addTx(20)
	c.counters(true).addTx(300)
	c.counters(true).addRx(70)

	webSocket, h1Plus, total := c.split()
	if webSocket.FramesTx != 2 || webSocket.BytesTx != 120 {
		t.Fatalf("websocket tx: %+v", webSocket)
	}
	if webSocket.FramesRx != 0 || webSocket.BytesRx != 0 {
		t.Fatalf("websocket rx must stay empty: %+v", webSocket)
	}
	if h1Plus.FramesTx != 1 || h1Plus.BytesTx != 300 || h1Plus.FramesRx != 1 || h1Plus.BytesRx != 70 {
		t.Fatalf("h1+ counters: %+v", h1Plus)
	}
	want := modeCounterValues{FramesTx: 3, FramesRx: 1, BytesTx: 420, BytesRx: 70}
	if total != want {
		t.Fatalf("total = %+v, want %+v", total, want)
	}
}

// TestH1CarrierCountersH1PlusOffStaysZero is the provider with H1+ turned off:
// every payload lands in the websocket pair and the H1+ pair never moves.
func TestH1CarrierCountersH1PlusOffStaysZero(t *testing.T) {
	var c h1CarrierCounters
	for i := 0; i < 5; i++ {
		c.counters(false).addTx(64)
		c.counters(false).addRx(32)
	}
	webSocket, h1Plus, total := c.split()
	if h1Plus != (modeCounterValues{}) {
		t.Fatalf("h1+ must stay zero with H1+ off: %+v", h1Plus)
	}
	if webSocket != total {
		t.Fatalf("websocket = %+v, total = %+v", webSocket, total)
	}
}

// TestH1CarrierCountersIgnoreEmptyFrames keeps keepalives out of the split, the
// same rule the aggregate counters already follow.
func TestH1CarrierCountersIgnoreEmptyFrames(t *testing.T) {
	var c h1CarrierCounters
	c.counters(false).addTx(0)
	c.counters(true).addRx(0)
	c.counters(true).addTx(-1)
	if webSocket, h1Plus, total := c.split(); webSocket != (modeCounterValues{}) ||
		h1Plus != (modeCounterValues{}) || total != (modeCounterValues{}) {
		t.Fatalf("empty frames must not count: ws=%+v h1+=%+v total=%+v", webSocket, h1Plus, total)
	}
}

// TestTransportModeStatsSplitsH1 exercises the package counters end to end: the
// snapshot's H1 fields are the sum of the two carrier kinds, and both kinds are
// visible beside the total.
func TestTransportModeStatsSplitsH1(t *testing.T) {
	before := TransportModeStats()
	h1ModeStats.counters(false).addTx(1000)
	h1ModeStats.counters(false).addRx(10)
	h1ModeStats.counters(true).addTx(500)
	h1ModeStats.counters(true).addRx(250)
	t.Cleanup(func() {
		// leave the process counters as found
		h1ModeStats.webSocket.framesTx.Add(^uint64(0))
		h1ModeStats.webSocket.bytesTx.Add(^uint64(999))
		h1ModeStats.webSocket.framesRx.Add(^uint64(0))
		h1ModeStats.webSocket.bytesRx.Add(^uint64(9))
		h1ModeStats.h1Plus.framesTx.Add(^uint64(0))
		h1ModeStats.h1Plus.bytesTx.Add(^uint64(499))
		h1ModeStats.h1Plus.framesRx.Add(^uint64(0))
		h1ModeStats.h1Plus.bytesRx.Add(^uint64(249))
	})

	got := TransportModeStats()
	if d := got.H1BytesTx - before.H1BytesTx; d != 1500 {
		t.Fatalf("h1 tx bytes delta = %d, want 1500", d)
	}
	if d := got.H1WebSocketBytesTx - before.H1WebSocketBytesTx; d != 1000 {
		t.Fatalf("websocket tx bytes delta = %d, want 1000", d)
	}
	if d := got.H1PlusBytesTx - before.H1PlusBytesTx; d != 500 {
		t.Fatalf("h1+ tx bytes delta = %d, want 500", d)
	}
	if got.H1BytesTx != got.H1WebSocketBytesTx+got.H1PlusBytesTx ||
		got.H1FramesTx != got.H1WebSocketFramesTx+got.H1PlusFramesTx ||
		got.H1BytesRx != got.H1WebSocketBytesRx+got.H1PlusBytesRx ||
		got.H1FramesRx != got.H1WebSocketFramesRx+got.H1PlusFramesRx {
		t.Fatalf("h1 total is not the sum of the kinds: %+v", got)
	}
}

// BenchmarkH1CarrierAddTx is the payload accounting a split connection does:
// one add on the counter pair picked when the upgrade settled.
func BenchmarkH1CarrierAddTx(b *testing.B) {
	var c h1CarrierCounters
	stats := c.counters(true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stats.addTx(1200)
	}
}

// BenchmarkH1AggregateAddTx is the accounting shape this replaces: the same add
// on the single process-wide pair.
func BenchmarkH1AggregateAddTx(b *testing.B) {
	stats := &h1DirectStats
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stats.addTx(1200)
	}
}
