// Copyright (c) 2026. No License.
//
// Regression test for forged undecryptable frames on P2P routes.
// These repos drop undecryptable frames before any session/sequence state
// is created, so forged source IDs must not allocate any peer state.

package connect

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

const forgedSourceFrameCount = 256

func TestUndecryptableFramesNoPeerState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	settings.ControlPingTimeout = 0

	barrierSource := NewId()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(barrierSource)
	defer client.Close()

	// inbound route for forged frames
	inbound := make(Route, forgedSourceFrameCount+8)
	client.RouteManager().UpdateTransportWithProperties(
		NewReceiveGatewayTransportWithType(TransportTypeP2p),
		[]Route{inbound},
		TransferCarrierProperties{},
	)

	// register a receive callback to catch the barrier
	delivered := make(chan string, 8)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			msg, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if simple, ok := msg.(*protocol.SimpleMessage); ok {
				select {
				case delivered <- simple.Content:
				default:
				}
			}
		}
	})

	// send forged undecryptable frames with distinct source IDs
	forgedIds := make(map[Id]bool)
	for range forgedSourceFrameCount {
		forgedId := NewId()
		forgedIds[forgedId] = true
		sessionRole := protocol.SequenceRole_SequenceRoleClient
		tfBytes, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath:           TransferPath{SourceId: forgedId, DestinationId: client.ClientId()}.ToProtobuf(),
			EncryptedTransferFrame: make([]byte, 32),
			SessionRole:            &sessionRole,
		})
		if err != nil {
			t.Fatal(err)
		}
		inbound <- tfBytes
	}

	// send barrier frame from a legitimate source
	barrierBytes := forgeBarrierFrame(t, barrierSource, client.ClientId(), "barrier")
	inbound <- barrierBytes

	// wait for barrier to be delivered
	select {
	case content := <-delivered:
		if content != "barrier" {
			t.Fatalf("unexpected barrier content: %s", content)
		}
	case <-client.Done():
		t.Fatal("client stopped before barrier")
	case <-time.After(30 * time.Second):
		t.Fatal("barrier not delivered")
	}

	// verify no peer state was created for forged IDs
	sessionCount := 0
	sendSeqCount := 0
	receiveSeqCount := 0

	// Check encryption sessions
	client.encryptionSessionManager.stateLock.Lock()
	for key := range client.encryptionSessionManager.sessions {
		if forgedIds[key.peerId] {
			sessionCount++
		}
	}
	client.encryptionSessionManager.stateLock.Unlock()

	// Check send sequences
	client.sendBuffer.mutex.Lock()
	for seqId := range client.sendBuffer.sendSequences {
		if forgedIds[seqId.Destination.SourceId] {
			sendSeqCount++
		}
	}
	client.sendBuffer.mutex.Unlock()

	// Check receive sequences
	client.receiveBuffer.mutex.Lock()
	for seqId := range client.receiveBuffer.receiveSequences {
		if forgedIds[seqId.Source.SourceId] {
			receiveSeqCount++
		}
	}
	client.receiveBuffer.mutex.Unlock()

	if sessionCount != 0 || sendSeqCount != 0 || receiveSeqCount != 0 {
		t.Fatalf("forged frames allocated peer state: sessions=%d sendSeq=%d receiveSeq=%d", sessionCount, sendSeqCount, receiveSeqCount)
	}
}

func forgeBarrierFrame(t *testing.T, sourceId Id, destinationId Id, content string) []byte {
	t.Helper()
	msgBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	pack := &protocol.Pack{
		MessageId:      NewId().Bytes(),
		SequenceId:     NewId().Bytes(),
		SequenceNumber: 0,
		Head:           true,
		Frames: []*protocol.Frame{
			{
				MessageType:  protocol.MessageType_TestSimpleMessage,
				MessageBytes: msgBytes,
			},
		},
	}
	tfBytes, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sourceId, DestinationId: destinationId}.ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tfBytes
}
