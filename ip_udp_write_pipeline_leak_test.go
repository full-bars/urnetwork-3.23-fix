package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestUdpSequenceWritePipelineDrainsBuffersOnCancel guards against a
// regression where `UdpSequence.Run`'s write-pipeline goroutine could leak
// pooled `ipPacket` buffers still queued in its internal `writePayloads`
// channel when the sequence was canceled. The write goroutine only returned
// the buffer it was actively writing; anything still buffered in the channel
// (queued by the forwarding loop but not yet dequeued) was dropped instead of
// returned to the message pool.
//
// The same drain defer is applied to `TcpSequence.Run`'s write pipeline
// (identical 14-line block, same goroutine pattern). An automated TCP test is
// impractical — it requires constructing valid SYN packets and a full TCP
// handshake — but the fix is code-review verified: both paths share the same
// structure, same variable names, and same defer placement.
//
// The race is inherently timing-dependent (map/channel select ordering), so
// this runs many iterations with a small channel capacity and an immediate
// cancel to make the window in which items are still queued at cancellation
// very likely to be hit at least once. The final assertion checks the
// message pool's cumulative taken/returned counts, not any single iteration.
func TestUdpSequenceWritePipelineDrainsBuffersOnCancel(t *testing.T) {
	ResetMessagePoolStats()

	udpBufferSettings := DefaultUdpBufferSettings()
	udpBufferSettings.SequenceBufferSize = 1
	udpBufferSettings.WriteTimeout = 2 * time.Second
	udpBufferSettings.IdleTimeout = 2 * time.Second

	// A real, reachable local UDP destination. UDP datagrams to a local
	// address are accepted by the kernel near-instantly regardless of
	// whether anything reads them, so no artificial slow-down is needed:
	// the only thing that matters for reproducing the leak is whether an
	// item is still sitting in the (size-1) `writePayloads` channel at the
	// moment `ctx` is canceled.
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("could not open udp listener: %s", err)
	}
	defer listener.Close()
	destAddr := listener.LocalAddr().(*net.UDPAddr)

	const iterations = 200
	const sendsPerIteration = 4

	for i := 0; i < iterations; i += 1 {
		ctx, cancel := context.WithCancel(context.Background())

		seq := NewUdpSequence(
			ctx,
			func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
			DestinationId(NewId()),
			protocol.ProvideMode_Network,
			4,
			net.ParseIP("127.0.0.1"), UDPPort(40000),
			destAddr.IP, UDPPort(destAddr.Port),
			udpBufferSettings,
		)

		done := make(chan struct{})
		go func() {
			seq.Run()
			close(done)
		}()

		// Enqueue a burst of sends without waiting for the write goroutine to
		// drain them, then cancel immediately - this is the exact window the
		// fix's drain-on-exit defer covers.
		for j := 0; j < sendsPerIteration; j += 1 {
			udp := &parsedUdp{}
			udp.payload = []byte("x")
			ipPacket := MessagePoolGet(2048)
			sendItem := &UdpSendItem{
				udp:      udp,
				ipPacket: ipPacket,
			}
			// `send` only hands off ownership of ipPacket when it actually
			// enqueues the item (success == true); a non-blocking send that
			// finds `sendItems` full does not take ownership, so the caller
			// must return it itself (same contract as every other send call
			// site in this package).
			if success, _ := seq.send(sendItem, 0); !success {
				MessagePoolReturn(ipPacket)
			}
		}
		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: UdpSequence.Run did not exit after cancel", i)
		}
	}

	stats := MessagePoolStats()
	ratio, ok := stats[2048][0]
	if !ok {
		t.Fatalf("no message pool stats recorded for size 2048 tag 0")
	}
	if ratio < 1.0 {
		t.Fatalf("leaked pooled buffers: return ratio = %f, want 1.0 (all taken buffers returned)", ratio)
	}
}
