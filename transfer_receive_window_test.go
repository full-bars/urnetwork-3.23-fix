package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The platform's capability probe classifies a provider as having resizable
// windows when the Acks it sends carry Ack.receive_window_byte_count, starting
// with the first. The value is the out-of-order hold capacity, never zero for
// a default receiver, and it must be the capacity the receiver enforces.

func TestReceiveWindowAdvertisementIsTheHoldCapacity(t *testing.T) {
	for _, capacity := range []ByteCount{kib(320), mib(2) + kib(512), mib(16)} {
		_, seq := newReceiveHoldTestSequence(t, ReceiveHoldCommittedPrefix, capacity)
		if got := seq.receiveWindowAdvertisement(); got != capacity {
			t.Fatalf("advertised %d, want the enforced hold capacity %d", got, capacity)
		}
	}
}

func TestReceiveWindowAdvertisementDefaultIsPositive(t *testing.T) {
	settings := DefaultReceiveBufferSettings()
	if settings.ReceiveQueueMaxByteCount <= 0 {
		t.Fatalf("the default receive hold capacity is %d; an advertised zero stalls upstream senders", settings.ReceiveQueueMaxByteCount)
	}
}

func TestReceiveWindowAdvertisementNeverNegative(t *testing.T) {
	_, seq := newReceiveHoldTestSequence(t, ReceiveHoldCommittedPrefix, -5)
	if got := seq.receiveWindowAdvertisement(); got != 0 {
		t.Fatalf("a negative capacity advertised %d, want 0", got)
	}
}

// The hand-rolled v2 Ack and the legacy proto Ack both decode with the field
// present, and a legacy decoder that skips field 8 still reads the rest.
func TestAckWithReceiveWindowDecodesWithTheFieldPresent(t *testing.T) {
	m := &sendAckFrame{
		path:                   TransferPath{DestinationId: NewId(), SourceId: NewId()},
		messageId:              NewId(),
		sequenceId:             NewId(),
		selective:              true,
		tag:                    &protocol.Tag{SendTime: 7},
		receiveWindowByteCount: 2_621_440,
		receiveWindowSet:       true,
	}
	wire := marshalSendAckTransferFrame(m)
	defer MessagePoolReturn(wire)
	var frame protocol.TransferFrame
	if err := proto.Unmarshal(wire, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Ack == nil || frame.Ack.ReceiveWindowByteCount == nil {
		t.Fatal("the Ack does not carry receive_window_byte_count")
	}
	if *frame.Ack.ReceiveWindowByteCount != 2_621_440 {
		t.Fatalf("decoded %d, want 2621440", *frame.Ack.ReceiveWindowByteCount)
	}
	if !frame.Ack.Selective || frame.Ack.Tag == nil || frame.Ack.Tag.SendTime != 7 {
		t.Fatalf("the other Ack fields changed: %v", frame.Ack)
	}
}

// End to end over two real clients: every Ack the receiving side puts on the
// wire, the first included, carries receive_window_byte_count equal to the
// hold capacity it enforces. The probe concludes from the first Ack it sees.
func TestReceiverAcksCarryTheReceiveWindowFromTheFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()

	aSend := make(chan []byte)
	bSend := make(chan []byte)
	aReceive := make(chan []byte)
	bReceive := make(chan []byte)

	type observedAck struct {
		window *uint64
	}
	var mutex sync.Mutex
	var observed []observedAck
	forward := func(in chan []byte, out chan []byte, sniff bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-in:
				if sniff {
					var frame protocol.TransferFrame
					if err := proto.Unmarshal(message, &frame); err == nil && frame.Ack != nil {
						mutex.Lock()
						observed = append(observed, observedAck{window: frame.Ack.ReceiveWindowByteCount})
						mutex.Unlock()
					}
				}
				select {
				case out <- message:
				case <-ctx.Done():
					return
				}
			}
		}
	}
	go forward(aSend, bReceive, false)
	go forward(bSend, aReceive, true)

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}
	newSide := func(clientId Id, send chan []byte, receive chan []byte) *Client {
		settings := DefaultClientSettings()
		settings.SendBufferSettings.SequenceBufferSize = 0
		settings.SendBufferSettings.AckBufferSize = 0
		settings.ReceiveBufferSettings.SequenceBufferSize = 0
		settings.ForwardBufferSettings.SequenceBufferSize = 0
		applyTestEncryptionSettings(settings, encryptionModeOff)
		client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
		client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{send})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{receive})
		client.ContractManager().SetProvideModes(provideModes)
		return client
	}
	a := newSide(aClientId, aSend, aReceive)
	defer a.Cancel()
	b := newSide(bClientId, bSend, bReceive)
	defer b.Cancel()

	received := make(chan struct{}, 64)
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for range frames {
			received <- struct{}{}
		}
	})

	err := a.ContractManager().HandleControlFrame(
		ContractKey{Destination: DestinationId(bClientId)},
		requireContractResult(
			protocol.ProvideMode_Network,
			b.ContractManager().RequireProvideSecretKey(protocol.ProvideMode_Network),
			aClientId,
			bClientId,
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	for i := 0; i < n; i++ {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: fmt.Sprintf("hi %d", i)}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !a.Send(frame, DestinationId(bClientId), func(err error) {}) {
			t.Fatal("send refused")
		}
	}
	for i := 0; i < n; i++ {
		select {
		case <-received:
		case <-time.After(30 * time.Second):
			t.Fatalf("received %d of %d messages", i, n)
		}
	}

	want := uint64(DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount)
	deadline := time.Now().Add(10 * time.Second)
	for {
		mutex.Lock()
		count := len(observed)
		mutex.Unlock()
		if 0 < count || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(observed) == 0 {
		t.Fatal("no Ack was observed on the wire")
	}
	for i, ack := range observed {
		if ack.window == nil {
			t.Fatalf("Ack %d of %d has no receive_window_byte_count; the first Ack decides the probe's reading", i, len(observed))
		}
		if *ack.window != want {
			t.Fatalf("Ack %d advertises %d, want the hold capacity %d", i, *ack.window, want)
		}
	}
}
