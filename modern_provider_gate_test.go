package connect

// The platform's modern provider gate (urnetwork/server,
// model/egress_modern_provider.go and FP2FIX.md "Modern provider gate")
// measures two capabilities on a real client-to-provider session and never
// reads a version, because most provider lineages carry custom versions:
//
//  1. end-to-end encryption negotiated with a post-quantum hybrid group
//     (TLS 1.3 X25519MLKEM768), and
//  2. resizable transfer windows: the provider's acks carry
//     `receive_window_byte_count`.
//
// A provider that fails either leaves the Quality and Speed buckets on the
// platform's next pass and serves Online only. This test pins both on the wire,
// so a regression fails the build here instead of demoting the fleet.

import (
	"bytes"
	"crypto/tls"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func TestModernProviderGate(t *testing.T) {
	t.Run("post-quantum hybrid is offered", func(t *testing.T) {
		for _, curve := range DefaultSequencePostQuantumCurves {
			if curve == tls.X25519MLKEM768 {
				return
			}
		}
		t.Fatalf("X25519MLKEM768 missing from DefaultSequencePostQuantumCurves: %v", DefaultSequencePostQuantumCurves)
	})

	t.Run("acks advertise a resizable receive window", func(t *testing.T) {
		idA, idB := NewId(), NewId()
		window := uint64(mib(2) + kib(512))
		m := &sendAckFrame{
			path:                   TransferPath{DestinationId: idA, SourceId: idB},
			messageId:              idB,
			sequenceId:             idA,
			selective:              true,
			tag:                    &protocol.Tag{SendTime: 1_700_000_000_000},
			receiveWindowByteCount: window,
			receiveWindowSet:       true,
		}
		frameBytes := marshalSendAckTransferFrame(m)
		defer MessagePoolReturn(frameBytes)
		var tf protocol.TransferFrame
		if err := proto.Unmarshal(frameBytes, &tf); err != nil {
			t.Fatalf("unmarshal: %s", err)
		}
		ack := tf.GetAck()
		if ack == nil || ack.ReceiveWindowByteCount == nil {
			t.Fatal("ack does not carry receive_window_byte_count: providers on this line would fail the modern provider gate")
		}
		if *ack.ReceiveWindowByteCount != window {
			t.Fatalf("ack advertises %d, built %d", *ack.ReceiveWindowByteCount, window)
		}
	})

	t.Run("a legacy ack decoder skips the field and keeps the rest", func(t *testing.T) {
		window := uint64(mib(2))
		messageId, sequenceId := NewId(), NewId()
		b, err := ProtoMarshal(&protocol.Ack{
			MessageId:              messageId.Bytes(),
			SequenceId:             sequenceId.Bytes(),
			Selective:              true,
			ReceiveWindowByteCount: &window,
		})
		if err != nil {
			t.Fatalf("marshal: %s", err)
		}
		decoded, ok := decodeAck(b)
		if !ok {
			t.Fatal("the legacy decoder rejected an ack carrying field 8")
		}
		if !decoded.Selective || !bytes.Equal(decoded.MessageId, messageId.Bytes()) {
			t.Fatal("the legacy decoder lost fields 1-3 while skipping field 8")
		}
	})

}
