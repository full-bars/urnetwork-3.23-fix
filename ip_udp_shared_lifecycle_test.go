package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The shared socket lifecycle is the provider profile's topology: a flow's
// socket is opened and registered when the flow is created, its datagrams are
// written directly under the flow's send lock, and no send goroutine, send
// queue, or per-flow idle timer exists. These tests drive real loopback udp
// sockets, because registration and the direct write are socket behavior.

// udpTestBufferSend is the family-specific send of Udp4Buffer / Udp6Buffer,
// whose shared generic core (UdpBuffer) the tests inspect.
type udpTestBufferSend func(
	source TransferPath,
	provideMode protocol.ProvideMode,
	udp *parsedUdp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error)

// testUdpSharedLifecycleSettings is the constrained provider profile's udp
// shape, without a memory target, so the enabling switch is explicit here.
func testUdpSharedLifecycleSettings() *UdpBufferSettings {
	settings := DefaultUdpBufferSettings()
	settings.Log = NewNoopLogger()
	settings.IdleTimeout = time.Hour
	settings.SequenceBufferSize = 16
	settings.SocketReadShardCount = 2
	settings.SharedSocketLifecycle = true
	return settings
}

// startUdpSharedLifecycleTestSend sends one datagram through the buffer. The
// pooled packet is returned on every failure path only, exactly as the local
// user nat does.
func startUdpSharedLifecycleTestSend(
	t *testing.T,
	send udpTestBufferSend,
	source TransferPath,
	sourceIp net.IP,
	sourcePort UDPPort,
	destinationIp net.IP,
	destinationPort uint16,
	payload []byte,
) {
	t.Helper()
	packet := MessagePoolGet(64)
	copy(packet, payload)
	udp := &parsedUdp{
		sourceIp:        sourceIp,
		destinationIp:   destinationIp,
		sourcePort:      sourcePort,
		destinationPort: UDPPort(destinationPort),
		payload:         packet[:len(payload)],
	}
	if success, err := send(source, protocol.ProvideMode_Network, udp, -1, packet); err != nil || !success {
		MessagePoolReturn(packet)
		t.Fatalf("send: success=%t err=%v", success, err)
	}
}

// A shared flow writes each datagram directly, and the readiness shard
// delivers every reply with its boundary intact. Order across the eight
// per-flow sockets is not asserted, because it is not defined.
func TestUdpBufferSharedSocketLifecycleSendsDirectlyNoLoss(t *testing.T) {
	echoPort := startUdpSocketReadTestEcho(t)
	settings := testUdpSharedLifecycleSettings()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bw := &ProxyBandwidth{}
	responses := make(chan []byte, 64)
	receive := func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		responses <- packet
	}
	buffer := NewUdp4Buffer(ctx, receive, settings, bw)
	t.Cleanup(func() {
		cancel()
		if buffer.socketReadPoller != nil {
			buffer.socketReadPoller.waitForLifecycle()
		}
	})

	source := SourceId(NewId())
	sourceIp := net.IPv4(192, 0, 2, 9).To4()
	destinationIp := net.IPv4(127, 0, 0, 1).To4()
	const packetCount = 8
	for i := range packetCount {
		startUdpSharedLifecycleTestSend(
			t,
			buffer.send,
			source,
			sourceIp,
			UDPPort(44000+i),
			destinationIp,
			echoPort,
			[]byte{byte(i)},
		)
	}

	if buffer.socketReadPoller == nil {
		t.Skip("socket readiness poller unavailable on this platform")
	}
	if len(buffer.socketReadPoller.shards) != settings.SocketReadShardCount {
		t.Fatalf("poll shards=%d, want %d",
			len(buffer.socketReadPoller.shards), settings.SocketReadShardCount)
	}

	buffer.mutex.Lock()
	if len(buffer.sequences) != packetCount {
		buffer.mutex.Unlock()
		t.Fatalf("sequences=%d, want %d", len(buffer.sequences), packetCount)
	}
	for _, sequence := range buffer.sequences {
		if !sequence.sharedSocketLifecycle {
			buffer.mutex.Unlock()
			t.Fatal("flow did not register its socket")
		}
		if sequence.sendItems != nil {
			buffer.mutex.Unlock()
			t.Fatal("shared flow retained a per-flow send queue")
		}
		if sequence.sharedSocket == nil || sequence.socketReadPollFd < 0 {
			buffer.mutex.Unlock()
			t.Fatal("shared flow has no registered socket")
		}
	}
	buffer.mutex.Unlock()

	// one datagram per write, with the exact payload length. The eight flows
	// have eight sockets read by two poller shards, so the order the replies
	// arrive in is not defined; what must hold is that every flow's datagram
	// comes back exactly once and with its boundary intact.
	seen := make(map[byte]bool, packetCount)
	for i := range packetCount {
		select {
		case packet := <-responses:
			expectedByteCount := Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize + 1
			if len(packet) != expectedByteCount {
				t.Fatalf("reply %d packet=%d bytes, want %d", i, len(packet), expectedByteCount)
			}
			marker := packet[len(packet)-1]
			if packetCount <= int(marker) {
				t.Fatalf("reply %d has unknown marker %d", i, marker)
			}
			if seen[marker] {
				t.Fatalf("reply %d repeats marker %d", i, marker)
			}
			seen[marker] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("reply %d timed out", i)
		}
	}
	if len(seen) != packetCount {
		t.Fatalf("got %d distinct markers, want %d", len(seen), packetCount)
	}

	// the direct write path is still bandwidth accounted: the wrapper credits
	// the send, and the raw readiness read credits the receive. Both count the
	// datagram payload, the same unit trackedConn.Read and .Write count on the
	// per-flow path; the synthesized IP header is not socket traffic.
	if bw.TotalTx.Load() != packetCount {
		t.Fatalf("total tx=%d, want %d", bw.TotalTx.Load(), packetCount)
	}
	if bw.TotalRx.Load() != uint64(packetCount) {
		t.Fatalf("total rx=%d, want %d", bw.TotalRx.Load(), packetCount)
	}
}

// A socket that cannot expose a descriptor (a proxy-wrapped conn) keeps the
// established per-flow reader, and the flow still carries traffic.
func TestUdpBufferSharedSocketLifecycleFallsBackToPerFlowReader(t *testing.T) {
	echoPort := startUdpSocketReadTestEcho(t)
	settings := testUdpSharedLifecycleSettings()
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// no SyscallConn: the readiness shards must refuse this socket
			return &udpSharedLifecycleOpaqueConn{Conn: conn}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	responses := make(chan byte, 8)
	buffer := NewUdp4Buffer(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		responses <- packet[len(packet)-1]
	}, settings, nil)

	source := SourceId(NewId())
	startUdpSharedLifecycleTestSend(
		t,
		buffer.send,
		source,
		net.IPv4(192, 0, 2, 10).To4(),
		UDPPort(44100),
		net.IPv4(127, 0, 0, 1).To4(),
		echoPort,
		[]byte{0x5a},
	)

	buffer.mutex.Lock()
	if len(buffer.sequences) != 1 {
		buffer.mutex.Unlock()
		t.Fatalf("sequences=%d, want 1", len(buffer.sequences))
	}
	var sequence *UdpSequence
	for _, activeSequence := range buffer.sequences {
		sequence = activeSequence
	}
	sharedLifecycle := sequence.sharedSocketLifecycle
	sendItemsPresent := sequence.sendItems != nil
	buffer.mutex.Unlock()
	if sharedLifecycle {
		t.Fatal("an opaque socket must not claim the shared lifecycle")
	}
	if !sendItemsPresent {
		t.Fatal("the fallback flow lost its per-flow send queue")
	}

	select {
	case marker := <-responses:
		if marker != 0x5a {
			t.Fatalf("fallback reply marker=%d", marker)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fallback reply timed out")
	}

	sequence.Close()
	if err := sequence.ctx.Err(); err == nil {
		t.Fatal("closing the flow did not cancel it")
	}
}

// udpSharedLifecycleOpaqueConn hides SyscallConn behind the embedded interface.
type udpSharedLifecycleOpaqueConn struct {
	net.Conn
}
