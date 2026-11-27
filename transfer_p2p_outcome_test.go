package connect

import (
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// A plaintext frame on a bound lane is counted decoded only when it is
// accepted. This client requires encryption for the source, so the fail-closed
// receive gate acks the plaintext pack and throws it away, and the frame is
// counted rejected instead of decoded.
func TestP2PSourceMismatchPlaintextRejectedWhenEncryptionRequired(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClientWithEncryption(t, TransportTypeP2p, peer, 2, EncryptionModeRequired)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, third, client.ClientId(), "stripped")

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchRejectedCount == 1
	}, "one rejected plaintext mismatch")
	expectNoDelivery(t, delivered)
	if stats := client.ReceiveStats(); stats.SourceMismatchCount != 1 ||
		stats.SourceMismatchDecodedCount != 0 ||
		stats.SourceMismatchDroppedCount != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// A plaintext pack that carries only encrypted control frames is taken by the
// per-peer session, not thrown away by the fail-closed gate: the gate discards
// application frames, and there are none. Such a pack counts decoded, while a
// pack that also carries an application frame is still discarded and counted
// rejected.
func TestP2PSourceMismatchControlOnlyPackDecoded(t *testing.T) {
	controlBytes, err := proto.Marshal(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		SessionRole: protocol.SequenceRole_SequenceRoleClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	appBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: "stripped"})
	if err != nil {
		t.Fatal(err)
	}
	control := &protocol.Frame{
		MessageType:  protocol.MessageType_TransferEncryptedControl,
		MessageBytes: controlBytes,
	}
	app := &protocol.Frame{
		MessageType:  protocol.MessageType_TestSimpleMessage,
		MessageBytes: appBytes,
	}

	cases := []struct {
		name    string
		frames  []*protocol.Frame
		decoded bool
	}{
		{"control only", []*protocol.Frame{control}, true},
		{"control and application", []*protocol.Frame{control, app}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSourcePolicy(t, "count", true)
			peer := NewId()
			client, route := newSourcePolicyClientWithEncryption(t, TransportTypeP2p, peer, 2, EncryptionModeRequired)
			delivered := deliverContent(client)
			third := NewId()
			client.ContractManager().AddNoContractPeer(third)

			route <- marshalTransferFrameForTest(t, &protocol.TransferFrame{
				TransferPath: TransferPath{SourceId: third, DestinationId: client.ClientId()}.ToProtobuf(),
				Pack: &protocol.Pack{
					MessageId:      NewId().Bytes(),
					SequenceId:     NewId().Bytes(),
					SequenceNumber: 0,
					Head:           true,
					Frames:         tc.frames,
				},
			})

			waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
				return stats.SourceMismatchCount == 1
			}, "one mismatch outcome")
			stats := client.ReceiveStats()
			if tc.decoded {
				if stats.SourceMismatchDecodedCount != 1 || stats.SourceMismatchRejectedCount != 0 {
					t.Fatalf("a control-only pack must count decoded, not rejected: %+v", stats)
				}
			} else {
				if stats.SourceMismatchRejectedCount != 1 || stats.SourceMismatchDecodedCount != 0 {
					t.Fatalf("a pack with an application frame must count rejected: %+v", stats)
				}
			}
			if stats.SourceMismatchDroppedCount != 0 {
				t.Fatalf("unexpected dropped outcome: %+v", stats)
			}
			expectNoDelivery(t, delivered)
		})
	}
}

// A frame that claims a third-party source and then fails to parse was never
// accepted, so it is counted rejected rather than decoded. Each case is a
// different way for the parse to fail after the path has been read.
func TestP2PSourceMismatchMalformedFrameIsRejected(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, source Id, destination Id) []byte
	}{
		{
			name: "bad pack protobuf",
			build: func(t *testing.T, source Id, destination Id) []byte {
				return marshalTransferFrameForTest(t, &protocol.TransferFrame{
					TransferPath: TransferPath{SourceId: source, DestinationId: destination}.ToProtobuf(),
					Frame: &protocol.Frame{
						MessageType:  protocol.MessageType_TransferPack,
						MessageBytes: []byte{0xff, 0xff, 0xff, 0xff},
					},
				})
			},
		},
		{
			name: "unknown message type",
			build: func(t *testing.T, source Id, destination Id) []byte {
				return marshalTransferFrameForTest(t, &protocol.TransferFrame{
					TransferPath: TransferPath{SourceId: source, DestinationId: destination}.ToProtobuf(),
					Frame: &protocol.Frame{
						MessageType:  protocol.MessageType(9999),
						MessageBytes: []byte{},
					},
				})
			},
		},
		{
			name: "empty envelope",
			build: func(t *testing.T, source Id, destination Id) []byte {
				return marshalTransferFrameForTest(t, &protocol.TransferFrame{
					TransferPath: TransferPath{SourceId: source, DestinationId: destination}.ToProtobuf(),
				})
			},
		},
		{
			name: "bad sequence id",
			build: func(t *testing.T, source Id, destination Id) []byte {
				return marshalTransferFrameForTest(t, &protocol.TransferFrame{
					TransferPath: TransferPath{SourceId: source, DestinationId: destination}.ToProtobuf(),
					Pack:         &protocol.Pack{SequenceId: []byte("bad")},
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSourcePolicy(t, "count", true)
			peer := NewId()
			client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
			delivered := deliverContent(client)
			third := NewId()
			client.ContractManager().AddNoContractPeer(third)

			route <- tc.build(t, third, client.ClientId())

			waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
				return stats.SourceMismatchRejectedCount == 1
			}, "one rejected malformed mismatch")
			expectNoDelivery(t, delivered)
			if stats := client.ReceiveStats(); stats.SourceMismatchDecodedCount != 0 {
				t.Fatalf("malformed frame counted decoded: %+v", stats)
			}
		})
	}
}

func marshalTransferFrameForTest(t *testing.T, transferFrame *protocol.TransferFrame) []byte {
	t.Helper()
	transferFrameBytes, err := proto.Marshal(transferFrame)
	if err != nil {
		t.Fatal(err)
	}
	return transferFrameBytes
}

func forgeBarrierFrameWithSequence(t *testing.T, sourceId Id, destinationId Id, sequenceId Id, content string) []byte {
	t.Helper()
	msgBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return marshalTransferFrameForTest(t, &protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sourceId, DestinationId: destinationId}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sequenceId.Bytes(),
			SequenceNumber: 0,
			Head:           true,
			Frames: []*protocol.Frame{
				{
					MessageType:  protocol.MessageType_TestSimpleMessage,
					MessageBytes: msgBytes,
				},
			},
		},
	})
}

// A mismatched pack whose sequence is older than the live head is discarded by
// ReceiveBuffer and must not count as decoded. An admitted pack still counts
// as decoded.
func TestP2PSourceMismatchOlderSequenceRejected(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 4)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	seqOld := Id{15: 1}
	seqNew := Id{15: 2}
	if !seqOld.LessThan(seqNew) {
		seqOld, seqNew = seqNew, seqOld
	}

	route <- forgeBarrierFrameWithSequence(t, third, client.ClientId(), seqNew, "current")
	expectDelivery(t, delivered, "current")
	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchDecodedCount == 1
	}, "one decoded mismatch for current sequence")

	route <- forgeBarrierFrameWithSequence(t, third, client.ClientId(), seqOld, "stale")
	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchCount == 2
	}, "second mismatch outcome for older sequence")
	if stats := client.ReceiveStats(); stats.SourceMismatchDecodedCount != 1 ||
		stats.SourceMismatchRejectedCount != 1 ||
		stats.SourceMismatchDroppedCount != 0 {
		t.Fatalf("unexpected stats after older sequence pack: %+v", stats)
	}
	select {
	case got := <-delivered:
		t.Fatalf("older sequence pack should not be delivered, got %q", got)
	default:
	}
}
