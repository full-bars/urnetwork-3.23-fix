package connect

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// multihopTestEndpoint is one end of a two-hop stream. Its receive lane is a
// real P2P receive transport over an in-memory connection, published bound to
// the adjacent hop the way P2pTransport.run publishes an endpoint lane. Its
// outgoing frames are captured on `out` for the relay to carry.
type multihopTestEndpoint struct {
	client    *Client
	out       Route
	delivered chan string
}

func newMultihopTestEndpoint(t *testing.T, ctx context.Context, relayId Id, streamId Id) (*multihopTestEndpoint, net.Conn) {
	t.Helper()
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(client.Close)

	out := make(Route, 64)
	client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeP2p), []Route{out})

	local, remote := net.Pipe()
	t.Cleanup(func() { local.Close(); remote.Close() })
	laneCtx, laneCancel := context.WithCancel(ctx)
	t.Cleanup(laneCancel)
	receiveTransport, receiveRoute := NewP2pReceiveTransport(laneCtx, laneCancel, local, streamId, DefaultP2pTransportSettings())
	client.RouteManager().UpdateTransportWithProperties(
		receiveTransport,
		[]Route{receiveRoute},
		TransferCarrierProperties{boundSourceId: relayId},
	)

	return &multihopTestEndpoint{
		client:    client,
		out:       out,
		delivered: deliverContent(client),
	}, remote
}

// runMultihopRelay carries frames from one endpoint to the other the way the
// transit hop's stream sequence does: bytes are forwarded unchanged through a
// P2P send transport, without passing through the relay's Client.run.
func runMultihopRelay(t *testing.T, ctx context.Context, from Route, to net.Conn, streamId Id) {
	t.Helper()
	laneCtx, laneCancel := context.WithCancel(ctx)
	t.Cleanup(laneCancel)
	_, sendRoute := NewP2pSendTransport(laneCtx, laneCancel, to, streamId, DefaultP2pTransportSettings())
	go func() {
		for {
			select {
			case <-laneCtx.Done():
				return
			case transferFrameBytes := <-from:
				select {
				case <-laneCtx.Done():
					MessagePoolReturn(transferFrameBytes)
					return
				case sendRoute <- transferFrameBytes:
				}
			}
		}
	}()
}

// runMultihopStream sends `count` messages from A to C through the relay B and
// reports how many C delivered and how many A saw acked.
func runMultihopStream(t *testing.T, count int) (delivered int, acked int, a *multihopTestEndpoint, c *multihopTestEndpoint) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relayId := NewId()
	streamId := NewId()
	a, aLane := newMultihopTestEndpoint(t, ctx, relayId, streamId)
	c, cLane := newMultihopTestEndpoint(t, ctx, relayId, streamId)
	a.client.ContractManager().AddNoContractPeer(c.client.ClientId())
	c.client.ContractManager().AddNoContractPeer(a.client.ClientId())

	// B: A -> C and C -> A (acks)
	runMultihopRelay(t, ctx, a.out, cLane, streamId)
	runMultihopRelay(t, ctx, c.out, aLane, streamId)

	acks := make(chan error, count)
	for i := 0; i < count; i += 1 {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: fmt.Sprintf("hop-%d", i)}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !a.client.SendWithTimeout(frame, DestinationId(c.client.ClientId()), func(err error) { acks <- err }, 5*time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("send %d not admitted", i)
		}
	}

	deadline := time.After(5 * time.Second)
	for delivered < count || acked < count {
		select {
		case <-c.delivered:
			delivered += 1
		case err := <-acks:
			if err == nil {
				acked += 1
			}
		case <-deadline:
			return delivered, acked, a, c
		}
	}
	return delivered, acked, a, c
}

// A legitimate two-hop stream A -> B -> C. The platform tells each endpoint
// only its adjacent hop (the server's stream key edges), so both endpoint
// lanes are bound to B, while every frame names its origin: the sender writes
// its own id as the source (SendSequence pack path), so A's packs name A and
// C's acks name C. The transit hop forwards on its own route managers and
// never reaches Client.run. Drop mode therefore discards the stream at both
// endpoints. This test records that behavior: it is the reason the default
// stays count. If it starts failing, drop mode no longer breaks multihop and
// the default can be revisited.
func TestP2PMultihopStreamBrokenInDropMode(t *testing.T) {
	setSourcePolicy(t, "drop", true)
	const count = 8
	delivered, acked, _, c := runMultihopStream(t, count)
	if delivered != 0 || acked != 0 {
		t.Fatalf("drop mode delivered %d/%d and acked %d/%d of a two-hop stream", delivered, count, acked, count)
	}
	if stats := c.client.ReceiveStats(); stats.SourceMismatchDroppedCount < count {
		t.Fatalf("the endpoint dropped %d mismatching frames, want at least %d: %+v", stats.SourceMismatchDroppedCount, count, stats)
	}
}

// The same harness in count and off mode, which deliver mismatching frames. It
// shows the harness carries a working stream, so a failure in drop mode is the
// policy and not the harness.
func TestP2PMultihopStreamWorksInCountAndOffMode(t *testing.T) {
	for _, mode := range []string{"count", "off"} {
		t.Run(mode, func(t *testing.T) {
			setSourcePolicy(t, mode, true)
			const count = 8
			delivered, acked, a, c := runMultihopStream(t, count)
			if delivered != count || acked != count {
				t.Fatalf("multihop stream in %s mode: delivered %d/%d, acked %d/%d; C stats %+v; A stats %+v",
					mode, delivered, count, acked, count, c.client.ReceiveStats(), a.client.ReceiveStats())
			}
		})
	}
}
