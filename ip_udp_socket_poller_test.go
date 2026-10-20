package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The readiness shards are infrastructure shared by the whole buffer, so their
// tests drive real loopback udp sockets: registration, ordered drain, shard
// fan-out, unregister, and cancellation each depend on a real descriptor.

// startUdpSocketReadTestEcho returns an echoing loopback udp endpoint. Each
// caller gets its own port, which is what gives the fixture more than one
// destination path to spread across shards.
func startUdpSocketReadTestEcho(t *testing.T) uint16 {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("udp echo listen: %s", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, addr, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			if _, writeErr := conn.WriteToUDP(buffer[:n], addr); writeErr != nil {
				return
			}
		}
	}()
	// one cleanup: the reader only returns once the listener is closed
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return uint16(conn.LocalAddr().(*net.UDPAddr).Port)
}

// startUdpSocketReadTestFlow dials one flow's socket and registers it with the
// poller, exactly as the shared lifecycle does.
func startUdpSocketReadTestFlow(
	t *testing.T,
	ctx context.Context,
	poller *udpSocketReadPoller,
	settings *UdpBufferSettings,
	sourcePort UDPPort,
	destinationPort UDPPort,
	receive ReceivePacketFunction,
) (*UdpSequence, net.Conn) {
	t.Helper()
	sequence := NewUdpSequence(
		ctx,
		receive,
		DestinationId(NewId()),
		protocol.ProvideMode_Network,
		4,
		net.IPv4(127, 0, 0, 1).To4(), UDPPort(sourcePort),
		net.IPv4(127, 0, 0, 1).To4(), UDPPort(destinationPort),
		settings,
	)
	socket, err := net.DialUDP(
		"udp",
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(sourcePort)},
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(destinationPort)},
	)
	if err != nil {
		t.Fatalf("udp dial: %s", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	if !poller.register(sequence, socket) {
		t.Fatalf("flow %d->%d was not registered", sourcePort, destinationPort)
	}
	return sequence, socket
}

// Every flow's datagrams must arrive once, in order, with the registered
// descriptor retired when the flow is unregistered.
func TestUdpSocketReadPollerDrainsFlowsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultUdpBufferSettings()
	settings.Log = NewNoopLogger()
	settings.IdleTimeout = time.Hour
	settings.SocketReadShardCount = 2
	poller := newUdpSocketReadPoller(ctx, settings)
	if poller == nil {
		t.Skip("socket readiness poller unavailable on this platform")
	}
	t.Cleanup(func() {
		cancel()
		poller.waitForLifecycle()
	})

	const packetCount = 16
	const maxFlows = 12
	type flowFixture struct {
		sequence *UdpSequence
		socket   net.Conn
		received chan byte
	}
	registeredShards := func() int {
		count := 0
		for i := range poller.shards {
			shard := &poller.shards[i]
			shard.mutex.RLock()
			registrationCount := len(shard.byFd)
			shard.mutex.RUnlock()
			if 0 < registrationCount {
				count += 1
			}
		}
		return count
	}
	flows := make([]*flowFixture, 0, maxFlows)
	// A flow's shard is a hash of its destination path, so six ephemeral echo
	// ports can all land on one shard. Keep adding flows until the fan-out is
	// real: this test is about ordering across shards, and the hash spread has
	// its own test.
	for i := 0; i < maxFlows; i++ {
		received := make(chan byte, packetCount)
		destinationPort := UDPPort(startUdpSocketReadTestEcho(t))
		sequence, socket := startUdpSocketReadTestFlow(
			t,
			ctx,
			poller,
			settings,
			UDPPort(41000+i),
			destinationPort,
			func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
				select {
				case received <- packet[len(packet)-1]:
				default:
				}
			},
		)
		flows = append(flows, &flowFixture{sequence: sequence, socket: socket, received: received})
		if registeredShards() == len(poller.shards) {
			break
		}
	}
	flowCount := len(flows)
	if shardCount := registeredShards(); shardCount != len(poller.shards) {
		t.Fatalf("registered shards=%d, want %d (fan-out exhausted)", shardCount, len(poller.shards))
	}

	for i, flow := range flows {
		for j := range packetCount {
			if _, err := flow.socket.Write([]byte{byte(i), byte(j)}); err != nil {
				t.Fatalf("flow %d write %d: %s", i, j, err)
			}
		}
	}
	for i, flow := range flows {
		for j := range packetCount {
			select {
			case marker := <-flow.received:
				if marker != byte(j) {
					t.Fatalf("flow %d datagram %d marker=%d", i, j, marker)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("flow %d datagram %d timed out", i, j)
			}
		}
	}

	// one flow retires while the others keep their registrations
	flow := flows[0]
	poller.unregister(flow.sequence)
	shard := &poller.shards[flow.sequence.socketReadPollShard]
	shard.mutex.RLock()
	_, registered := shard.byFd[int(flow.socket.(*net.UDPConn).LocalAddr().(*net.UDPAddr).Port)]
	shard.mutex.RUnlock()
	if registered {
		t.Fatal("unregistered flow kept its descriptor registration")
	}
	if flow.sequence.socketReadPollFd != -1 {
		t.Fatalf("unregistered flow poll fd=%d, want -1", flow.sequence.socketReadPollFd)
	}
	for i := 1; i < flowCount; i += 1 {
		if flows[i].sequence.socketReadPollFd < 0 {
			t.Fatalf("flow %d lost its registration", i)
		}
	}
}

// A flow must always resolve to the same shard for its whole life, and distinct
// destination paths must be able to reach every shard.
func TestUdpSocketReadPollShardIndexIsStableAndSpread(t *testing.T) {
	settings := DefaultUdpBufferSettings()
	settings.SocketReadShardCount = 4
	sequence := NewUdpSequence(
		context.Background(),
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		DestinationId(NewId()),
		protocol.ProvideMode_Network,
		4,
		net.IPv4(192, 0, 2, 1).To4(), UDPPort(42000),
		net.IPv4(198, 51, 100, 1).To4(), UDPPort(43000),
		settings,
	)
	first := udpSocketReadShardIndex(sequence, settings.SocketReadShardCount)
	if first < 0 || settings.SocketReadShardCount <= first {
		t.Fatalf("shard index %d is out of range", first)
	}
	for range 100 {
		if got := udpSocketReadShardIndex(sequence, settings.SocketReadShardCount); got != first {
			t.Fatalf("shard index changed from %d to %d", first, got)
		}
	}

	reached := map[int]bool{}
	for port := 43000; port < 43004; port += 1 {
		sequence.destinationPort = UDPPort(port)
		reached[udpSocketReadShardIndex(sequence, settings.SocketReadShardCount)] = true
	}
	if len(reached) != 4 {
		t.Fatalf("distinct destinations reached %d of 4 shards", len(reached))
	}
}

// Cancellation must stop every worker and return ownership of every packet that
// was still queued for delivery.
func TestUdpSocketReadPollerCancellationReturnsQueuedPackets(t *testing.T) {
	const packetSize = 2048
	ResetMessagePoolStats()
	baseTaken, baseReturned := waitForPoolBalance(t, packetSize)

	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultUdpBufferSettings()
	settings.Log = NewNoopLogger()
	settings.SequenceBufferSize = 16
	settings.SocketReadShardCount = 1
	poller := newUdpSocketReadPoller(ctx, settings)
	if poller == nil {
		cancel()
		t.Skip("socket readiness poller unavailable on this platform")
	}
	sequence := NewUdpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		DestinationId(NewId()),
		protocol.ProvideMode_Network,
		4,
		net.IPv4(192, 0, 2, 1).To4(), UDPPort(42100),
		net.IPv4(198, 51, 100, 1).To4(), UDPPort(43100),
		settings,
	)
	shard := &poller.shards[0]
	for range 4 {
		packet := MessagePoolGet(packetSize)
		if !shard.enqueue(sequence, packet) {
			MessagePoolReturn(packet)
			t.Fatal("delivery queue refused a packet while the buffer was live")
		}
	}
	cancel()
	poller.waitForLifecycle()

	taken, returned := waitForPoolBalance(t, packetSize)
	if taken-baseTaken != returned-baseReturned {
		t.Fatalf("canceled poller unbalance: taken_delta=%d returned_delta=%d",
			taken-baseTaken, returned-baseReturned)
	}
}
