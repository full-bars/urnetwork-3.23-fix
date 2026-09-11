package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Deterministic reproductions of the pinned-provider collapse: an ordered
// sequence striped across a lossy unreliable lane (WebRTC datagram fast path)
// and a reliable relay lane. Both tests drive the real Client send/receive
// pumps through in-memory routes; nothing here depends on timing except the
// bounded waits for a pump to act.

// Builds a sender with one unreliable route and one reliable route to the same
// peer. The unreliable flight admits exactly one byte, so the first Pack that
// rides it fills the flight; the reliable route is unbuffered, so nothing can
// leak onto it unless the test reads it. Resend intervals are short so a
// timeout on the unreliable lane happens within the test.
func newMixedLaneFlightTestClient(
	t *testing.T,
) (*Client, Id, Route, Route, Route, <-chan sendSequenceId) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.UnreliableInitialFlightByteCount = 1
	settings.SendBufferSettings.UnreliableMinimumFlightByteCount = 1
	settings.SendBufferSettings.UnreliableMaximumFlightByteCount = 1
	settings.SendBufferSettings.UnreliableFlightIncreaseByteCount = 1
	settings.SendBufferSettings.MinResendInterval = 100 * time.Millisecond
	settings.SendBufferSettings.RttMinResendInterval = 100 * time.Millisecond
	settings.SendBufferSettings.MaxResendInterval = 250 * time.Millisecond
	settings.SendBufferSettings.UnreliableMaxResendInterval = 250 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	peerId := NewId()
	client.ContractManager().AddNoContractPeer(peerId)

	unreliableRoute := make(Route, 16)
	reliableRoute := make(Route)
	fromPeer := make(Route, 16)
	waits := make(chan sendSequenceId, 16)
	client.sendBuffer.beforeResendCapacityWaitForTest = func(sequenceId sendSequenceId) {
		select {
		case waits <- sequenceId:
		default:
		}
	}
	client.RouteManager().UpdateTransportWithProperties(
		NewSendGatewayTransportWithType(TransportTypeP2p),
		[]Route{unreliableRoute},
		TransferCarrierProperties{Unreliable: true},
	)
	client.RouteManager().UpdateTransportWithProperties(
		NewSendGatewayTransportWithType(TransportTypeH1),
		[]Route{reliableRoute},
		TransferCarrierProperties{},
	)
	client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fromPeer})
	t.Cleanup(func() {
		cancel()
		client.Close()
		for _, route := range []Route{unreliableRoute, fromPeer} {
			for {
				select {
				case message := <-route:
					MessagePoolReturn(message)
				default:
					goto next
				}
			}
		next:
		}
	})
	return client, peerId, unreliableRoute, reliableRoute, fromPeer, waits
}

// Sends one application frame whose ownership transfers on success.
func sendTransferFlightTestMessage(t *testing.T, client *Client, peerId Id, index int) {
	sendTransferFlightTestMessageWithOptions(t, client, peerId, index)
}

func sendTransferFlightTestMessageWithOptions(
	t *testing.T,
	client *Client,
	peerId Id,
	index int,
	opts ...any,
) {
	t.Helper()
	frame, err := ToFrame(
		&protocol.SimpleMessage{Content: fmt.Sprintf("flight-%d", index)},
		DefaultProtocolVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !client.SendWithTimeout(frame, DestinationId(peerId), nil, 5*time.Second, opts...) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("send flight message %d was not admitted", index)
	}
}

// Takes one routed Pack and returns its decoded copy after releasing the
// pooled carrier bytes.
func takeTransferFlightTestPack(t *testing.T, route Route) *protocol.Pack {
	t.Helper()
	select {
	case transferFrameBytes := <-route:
		defer MessagePoolReturn(transferFrameBytes)
		var transferFrame protocol.TransferFrame
		if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
			t.Fatalf("decode flight TransferFrame: %v", err)
		}
		if transferFrame.Pack != nil {
			return transferFrame.Pack
		}
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
			t.Fatalf("flight route carried %v, want Transfer Pack", frame)
		}
		pack := &protocol.Pack{}
		if err := ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
			t.Fatalf("decode flight Pack: %v", err)
		}
		return pack
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for flight Pack")
		return nil
	}
}

// Decodes one routed Pack and releases the pooled carrier bytes.
func decodeTransferFlightTestPack(t *testing.T, transferFrameBytes []byte) *protocol.Pack {
	t.Helper()
	defer MessagePoolReturn(transferFrameBytes)
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
		t.Fatalf("decode TransferFrame: %v", err)
	}
	if transferFrame.Pack != nil {
		return transferFrame.Pack
	}
	frame := transferFrame.GetFrame()
	if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
		t.Fatalf("route carried %v, want Transfer Pack", frame)
	}
	pack := &protocol.Pack{}
	if err := ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
		t.Fatalf("decode Pack: %v", err)
	}
	return pack
}

// The root cause of the collapse: one unacknowledged Pack on the unreliable
// lane filled the flight, and the flight gated every send of the sequence, so
// a sequence with an idle reliable lane sat at zero. Now the sequence keeps
// admitting Packs and writes them on the reliable lane only, and when the
// unreliable Pack times out it is re-sent on the reliable lane and released
// from the flight.
func TestSendSequenceFullUnreliableFlightOverflowsOntoReliableLane(t *testing.T) {
	client, peerId, unreliableRoute, reliableRoute, _, waits := newMixedLaneFlightTestClient(t)

	// the first Pack rides the unreliable lane (the only route with room) and
	// is never acknowledged: the one-byte flight is now full
	sendTransferFlightTestMessage(t, client, peerId, 0)
	firstPack := takeTransferFlightTestPack(t, unreliableRoute)
	firstSequenceNumber := firstPack.GetSequenceNumber()

	// every later Pack must be delivered without the sequence ever waiting on
	// the full unreliable flight. On stock, the flight gates every send of the
	// sequence: the reliable lane sits idle, the wait barrier fires, and the
	// overflow never arrives (the "1 Mb/s / 0 and never recovers" collapse).
	const overflowCount = 4
	for i := 1; i <= overflowCount; i++ {
		sendTransferFlightTestMessage(t, client, peerId, i)
	}
	// Packs coalesce frames, so count the application messages themselves.
	seen := map[string]bool{}
	reliableDelivered := 0
	collect := func(pack *protocol.Pack, reliable bool) {
		if pack.GetSequenceNumber() <= firstSequenceNumber {
			return
		}
		for _, frame := range pack.GetFrames() {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			simple, ok := message.(*protocol.SimpleMessage)
			if !ok {
				continue
			}
			if !seen[simple.Content] {
				seen[simple.Content] = true
				if reliable {
					reliableDelivered++
				}
			}
		}
	}
	deadline := time.After(15 * time.Second)
	for len(seen) < overflowCount {
		select {
		case transferFrameBytes := <-reliableRoute:
			collect(decodeTransferFlightTestPack(t, transferFrameBytes), true)
		case transferFrameBytes := <-unreliableRoute:
			// once the flight has room again a retransmit may ride either lane;
			// that still counts as delivery. The regression is the stall, below.
			collect(decodeTransferFlightTestPack(t, transferFrameBytes), false)
		case sequenceId := <-waits:
			t.Fatalf("sequence %v waited on the full unreliable flight although a reliable lane is active", sequenceId)
		case <-deadline:
			t.Fatalf("only %d of %d overflow messages were delivered (%v); the full unreliable flight stalled the sequence", len(seen), overflowCount, seen)
		}
	}
	// the overflow reached the reliable lane, not only via unreliable retransmits
	if reliableDelivered == 0 {
		t.Fatal("no overflow message was carried by the reliable lane")
	}
	if recovery := client.SendRecoveryStats(); recovery.UnreliableFlightWaitCount != 0 {
		t.Fatalf("sequence waited on the unreliable flight %d times with a reliable lane active: %+v", recovery.UnreliableFlightWaitCount, recovery)
	}
}

// The second penalty: a receiver replied to Packs that arrived on the
// unreliable lane with ACKs on that same lane, where the provider's UDP socket
// dropped them and every lost cumulative ACK timed out the sender's whole
// window. An ACK for an unreliable-carried Pack must leave on the reliable
// lane while one is active.
func TestReceiveSequenceAcksUnreliableCarriedPacksOnReliableLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	peerId := NewId()
	client.ContractManager().AddNoContractPeer(peerId)

	unreliableOut := make(Route, 16)
	reliableOut := make(Route, 16)
	unreliableIn := make(Route, 16)
	client.RouteManager().UpdateTransportWithProperties(
		NewSendGatewayTransportWithType(TransportTypeP2p),
		[]Route{unreliableOut},
		TransferCarrierProperties{Unreliable: true},
	)
	client.RouteManager().UpdateTransportWithProperties(
		NewSendGatewayTransportWithType(TransportTypeH1),
		[]Route{reliableOut},
		TransferCarrierProperties{},
	)
	client.RouteManager().UpdateTransportWithProperties(
		NewReceiveGatewayTransportWithType(TransportTypeP2p),
		[]Route{unreliableIn},
		TransferCarrierProperties{Unreliable: true},
	)
	t.Cleanup(func() {
		cancel()
		client.Close()
		for _, route := range []Route{unreliableOut, reliableOut, unreliableIn} {
			for {
				select {
				case message := <-route:
					MessagePoolReturn(message)
				default:
					goto next
				}
			}
		next:
		}
	})

	received := make(chan struct{}, 1)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		select {
		case received <- struct{}{}:
		default:
		}
	})

	// one Pack from the peer arrives on the unreliable lane
	frame, err := ToFrame(&protocol.SimpleMessage{Content: "via-unreliable"}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	messageId := NewId()
	sequenceId := NewId()
	packBytes, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{
			SourceId:      peerId,
			DestinationId: client.ClientId(),
		}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:      messageId.Bytes(),
			SequenceId:     sequenceId.Bytes(),
			SequenceNumber: 0,
			Head:           true,
			Frames:         []*protocol.Frame{frame},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case unreliableIn <- packBytes:
	case <-time.After(5 * time.Second):
		t.Fatal("could not deliver the Pack")
	}
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the Pack was not delivered to the receive callback")
	}

	// its ACK must ride the reliable lane; the client's own outbound frames
	// (e.g. its key exchange to the peer) may use either lane and are ignored
	isAck := func(transferFrameBytes []byte) (bool, *protocol.Ack) {
		var transferFrame protocol.TransferFrame
		if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if ack := transferFrame.GetAck(); ack != nil {
			return true, ack
		}
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferAck {
			return false, nil
		}
		ack := &protocol.Ack{}
		if err := ProtoUnmarshal(frame.MessageBytes, ack); err != nil {
			t.Fatalf("decode ACK: %v", err)
		}
		return true, ack
	}
	reliableAcks := 0
	deadline := time.After(10 * time.Second)
	settle := time.NewTimer(time.Hour)
	defer settle.Stop()
	for {
		select {
		case transferFrameBytes := <-reliableOut:
			ok, ack := isAck(transferFrameBytes)
			MessagePoolReturn(transferFrameBytes)
			if !ok {
				continue
			}
			if ackMessageId, err := IdFromBytes(ack.MessageId); err != nil || ackMessageId != messageId {
				t.Fatalf("reliable-lane ACK names message %v, want %v (err %v)", ackMessageId, messageId, err)
			}
			reliableAcks++
			if reliableAcks == 1 {
				// give any stray copy time to appear on the unreliable lane
				settle.Reset(300 * time.Millisecond)
			}
		case transferFrameBytes := <-unreliableOut:
			ok, _ := isAck(transferFrameBytes)
			MessagePoolReturn(transferFrameBytes)
			if ok {
				t.Fatal("an ACK for an unreliable-carried Pack was written to the unreliable lane")
			}
		case <-settle.C:
			return
		case <-deadline:
			t.Fatal("no ACK was written on the reliable lane")
		}
	}
}
