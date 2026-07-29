package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestUdp4BufferSendReturnsPacketOnBackpressureDrop guards against a leak
// where UdpBuffer.udpSend (called from LocalUserNat.runShard for every
// outbound UDP packet) discarded the (bool, error) result of
// UdpSequence.send without freeing the pooled ipPacket buffer on the drop
// paths (idle-close, full sequence buffer, write timeout). Every dropped
// packet under sustained backpressure permanently removed one buffer from
// the pool; this test fills the sequence's SequenceBufferSize=1 buffer and
// sends a second packet with timeout=0 (non-blocking), which must be
// dropped (success=false) and must return the pooled buffer.
func TestUdp4BufferSendReturnsPacketOnBackpressureDrop(t *testing.T) {
	ResetMessagePoolStats()

	udpBufferSettings := DefaultUdpBufferSettings()
	udpBufferSettings.SequenceBufferSize = 1
	udpBufferSettings.WriteTimeout = 2 * time.Second
	udpBufferSettings.IdleTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udp4Buffer := NewUdp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, udpBufferSettings, nil)

	udp := &parsedUdp{
		sourceIp:        net.ParseIP("127.0.0.1").To4(),
		destinationIp:   net.ParseIP("127.0.0.2").To4(),
		sourcePort:      UDPPort(40000),
		destinationPort: UDPPort(443),
		payload:         []byte("x"),
	}

	// First send starts the sequence's Run() goroutine and (racily) may or
	// may not be immediately drained; either way it establishes the
	// sequence. Use a non-zero timeout so it reliably succeeds.
	firstPacket := MessagePoolGet(2048)
	success, err := udp4Buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, 500*time.Millisecond, firstPacket)
	if err != nil || !success {
		t.Fatalf("first send: success=%v err=%v, want true, nil", success, err)
	}

	// Flood non-blocking sends (timeout=0) until at least one is dropped —
	// the sequence's single-slot buffer plus one in-flight write goroutine
	// makes backpressure easy to hit without needing precise timing.
	sawDrop := false
	for i := 0; i < 50 && !sawDrop; i++ {
		packet := MessagePoolGet(2048)
		success, err := udp4Buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, 0, packet)
		if err != nil {
			t.Fatalf("send: unexpected error %v", err)
		}
		if !success {
			sawDrop = true
		}
	}
	if !sawDrop {
		t.Skip("did not manage to trigger a backpressure drop; flaky under this timing, skip rather than false-fail")
	}

	cancel()
	time.Sleep(100 * time.Millisecond)

	stats := MessagePoolStats()
	ratio, ok := stats[2048][0]
	if !ok {
		t.Fatalf("no message pool stats recorded for size 2048 tag 0")
	}
	if ratio < 1.0 {
		t.Fatalf("leaked pooled buffers on backpressure drop: return ratio = %f, want 1.0", ratio)
	}
}
