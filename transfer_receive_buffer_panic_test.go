package connect

// Regression tests for two panics in ReceiveBuffer.Pack that a peer's packs could
// reach. Pack runs on the client's receive loop, which runs under HandleError with
// the client's cancel, so a panic there stops the whole client for every peer.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newPackPanicFixture(t *testing.T) (*ReceiveBuffer, *ReceivePack, receiveSequenceId, receiveSequenceHeadKey, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	t.Cleanup(client.Cancel)
	buffer := NewReceiveBuffer(ctx, client, DefaultReceiveBufferSettings())

	source := SourceId(NewId())
	sequenceId := NewId()
	receivePack := &ReceivePack{
		Source:     source,
		SequenceId: sequenceId,
		Pack:       &protocol.Pack{},
		Ctx:        ctx,
	}
	id := receiveSequenceId{
		Source:              receivePack.Source,
		SequenceId:          receivePack.SequenceId,
		EncryptionRole:      receivePack.EncryptionRole,
		EncryptionCompanion: receivePack.EncryptionCompanion,
	}
	headKey := receiveSequenceHeadKey{
		Source:              id.Source,
		EncryptionRole:      id.EncryptionRole,
		EncryptionCompanion: id.EncryptionCompanion,
	}
	return buffer, receivePack, id, headKey, ctx
}

// The head index names this pack's own sequence id but no live sequence has it: a
// stale index. It used to panic ("upgrade older sequence = "); it must heal the
// index and carry on.
func TestReceiveBufferPackHealsAStaleHeadIndex(t *testing.T) {
	buffer, receivePack, id, headKey, _ := newPackPanicFixture(t)
	buffer.mutex.Lock()
	buffer.headReceiveSequenceIds[headKey] = id
	buffer.mutex.Unlock()

	done := make(chan struct{})
	var panicked interface{}
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		buffer.Pack(receivePack, 50*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Pack did not return")
	}
	if panicked != nil {
		t.Fatalf("Pack panicked on a stale head index: %v", panicked)
	}
}

// A live sequence exists under this pack's id, but the lane head names another id.
// The retry path used to panic ("incorrect head sequence"); the pack must be dropped.
func TestReceiveBufferPackDropsAPackWhoseHeadIsAnotherLane(t *testing.T) {
	buffer, receivePack, id, headKey, ctx := newPackPanicFixture(t)

	// a sequence that is already closed, so handing it the pack fails and Pack retries
	closedCtx, closeSequence := context.WithCancel(ctx)
	sequence := NewReceiveSequence(
		closedCtx,
		buffer.client,
		receivePack.Source,
		receivePack.SequenceId,
		receivePack.EncryptionRole,
		receivePack.EncryptionCompanion,
		buffer.receiveBufferSettings,
	)
	closeSequence()
	buffer.mutex.Lock()
	buffer.receiveSequences[id] = sequence
	buffer.headReceiveSequenceIds[headKey] = receiveSequenceId{
		Source:              id.Source,
		SequenceId:          NewId(), // another sequence owns the lane head
		EncryptionRole:      id.EncryptionRole,
		EncryptionCompanion: id.EncryptionCompanion,
	}
	buffer.mutex.Unlock()

	done := make(chan struct{})
	var panicked interface{}
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		buffer.Pack(receivePack, 50*time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Pack did not return")
	}
	if panicked != nil {
		t.Fatalf("Pack panicked on a pack whose lane head is another sequence: %v", panicked)
	}
}

// A sealed frame with no path used to dereference nil in TransferPathFromProtobuf and
// panic the receive loop. A missing path is an error.
func TestTransferPathFromProtobufRefusesAMissingPath(t *testing.T) {
	if _, err := TransferPathFromProtobuf(nil); err == nil {
		t.Fatal("a missing path was accepted")
	}
}
