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
