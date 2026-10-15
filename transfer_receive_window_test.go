package connect

import (
	"testing"

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
