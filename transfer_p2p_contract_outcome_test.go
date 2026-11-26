package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func forgePackFrame(t *testing.T, sourceId Id, destinationId Id, sequenceId Id, sequenceNumber uint64, head bool, content string) []byte {
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
			SequenceNumber: sequenceNumber,
			Head:           head,
			Frames: []*protocol.Frame{
				{
					MessageType:  protocol.MessageType_TestSimpleMessage,
					MessageBytes: msgBytes,
				},
			},
		},
	})
}

// Close rejects unread mismatch packs without changing an earlier outcome.
func TestP2PSourceMismatchHandoffDrainRejected(t *testing.T) {
	client := &Client{}
	_, cancel := context.WithCancel(context.Background())
	seq := &ReceiveSequence{cancel: cancel, packs: make(chan *ReceivePack, 3)}
	unread := newP2PSourceMismatch(client, p2pSourceModeCount)
	decided := newP2PSourceMismatch(client, p2pSourceModeCount)
	decided.decoded()
	seq.packs <- &ReceivePack{sourceMismatch: unread}
	seq.packs <- &ReceivePack{sourceMismatch: decided}
	seq.packs <- &ReceivePack{}
	seq.Close()
	if got := client.p2pSources.mismatchAt(p2pSourceModeCount, p2pSourceOutcomeRejected); got != 1 {
		t.Fatalf("drained mismatch rejected = %d, want 1", got)
	}
	if got := client.p2pSources.mismatchAt(p2pSourceModeCount, p2pSourceOutcomeDecoded); got != 1 {
		t.Fatalf("previously decoded = %d, want 1", got)
	}
}

// A forged sender with no contract gets its pack admitted to the receive
// buffer, and then the receive sequence drops it for having no contract. The
// frame was never delivered, so it is counted rejected, not decoded.
func TestP2PSourceMismatchNoContractRejected(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	delivered := deliverContent(client)
	forged := NewId()

	route <- forgeBarrierFrame(t, forged, client.ClientId(), "no contract")

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchCount == 1
	}, "one mismatch outcome")
	expectNoDelivery(t, delivered)
	if stats := client.ReceiveStats(); stats.SourceMismatchRejectedCount != 1 ||
		stats.SourceMismatchDecodedCount != 0 ||
		stats.SourceMismatchDroppedCount != 0 {
		t.Fatalf("a no-contract pack must count rejected, not decoded: %+v", stats)
	}
}

// A forged pack held out of order waits in the receive queue. When the head of
// its sequence fails the contract check the sequence ends and drops the held
// pack too. Neither was delivered, so both are counted rejected.
func TestP2PSourceMismatchHeldNoContractRejected(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	delivered := deliverContent(client)
	forged := NewId()
	sequenceId := NewId()

	route <- forgePackFrame(t, forged, client.ClientId(), sequenceId, 1, false, "held")
	route <- forgePackFrame(t, forged, client.ClientId(), sequenceId, 0, true, "head")

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchCount == 2
	}, "two mismatch outcomes")
	expectNoDelivery(t, delivered)
	if stats := client.ReceiveStats(); stats.SourceMismatchRejectedCount != 2 ||
		stats.SourceMismatchDecodedCount != 0 ||
		stats.SourceMismatchDroppedCount != 0 {
		t.Fatalf("held and head no-contract packs must count rejected: %+v", stats)
	}
}
