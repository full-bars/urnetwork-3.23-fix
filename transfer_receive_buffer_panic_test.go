package connect

// Regression tests inject inconsistent receive-buffer indexes directly.
// Pack runs on the client's receive loop, so a panic can stop the client.

import (
	"context"
	"runtime"
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

// Keep one reference to detect both a missing return and an extra return.
func trackPackOwnership(pack *ReceivePack) []byte {
	pack.TransferFrameBytes = MessagePoolGet(128)
	return MessagePoolShareReadOnly(pack.TransferFrameBytes)
}

func assertPackOwnershipReleased(t *testing.T, retained []byte) {
	t.Helper()
	if !MessagePoolReturn(retained) {
		t.Fatal("pack ownership was not returned exactly once")
	}
}

func finishInstalledSequence(t *testing.T, buffer *ReceiveBuffer, id receiveSequenceId, headKey receiveSequenceHeadKey) {
	t.Helper()
	buffer.mutex.Lock()
	sequence := buffer.receiveSequences[id]
	if sequence == nil || buffer.headReceiveSequenceIds[headKey] != id {
		buffer.mutex.Unlock()
		t.Fatal("replacement sequence was not installed as head")
	}
	buffer.mutex.Unlock()
	sequence.Cancel()
	sequence.WaitForExit()
	deadline := time.Now().Add(10 * time.Second)
	for {
		buffer.mutex.Lock()
		finished := buffer.receiveSequences[id] != sequence
		buffer.mutex.Unlock()
		if finished {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sequence cleanup did not finish")
		}
		runtime.Gosched()
	}
}

// The head index names this pack's own sequence id but no live sequence has it: a
// stale index. It used to panic ("upgrade older sequence = "); it must heal the
// index and carry on.
func TestReceiveBufferPackHealsAStaleHeadIndex(t *testing.T) {
	buffer, receivePack, id, headKey, _ := newPackPanicFixture(t)
	buffer.mutex.Lock()
	buffer.headReceiveSequenceIds[headKey] = id
	buffer.mutex.Unlock()

	retained := trackPackOwnership(receivePack)
	success, err := buffer.Pack(receivePack, time.Second)
	if !success || err != nil {
		t.Fatalf("Pack: success=%v err=%v", success, err)
	}
	finishInstalledSequence(t, buffer, id, headKey)
	assertPackOwnershipReleased(t, retained)
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

	retained := trackPackOwnership(receivePack)
	logger := &lockCheckingLogger{buffer: buffer}
	buffer.log = logger
	success, err := buffer.Pack(receivePack, time.Second)
	if !success || err != nil {
		t.Fatalf("Pack: success=%v err=%v", success, err)
	}
	if !logger.unlocked {
		t.Fatal("mismatch diagnostic did not run outside the buffer lock")
	}
	buffer.mutex.Lock()
	if buffer.receiveSequences[id] != sequence || buffer.headReceiveSequenceIds[headKey] == id {
		buffer.mutex.Unlock()
		t.Fatal("drop changed the existing sequence or head")
	}
	buffer.mutex.Unlock()
	assertPackOwnershipReleased(t, retained)
}

// A sealed frame with no path used to dereference nil in TransferPathFromProtobuf and
// panic the receive loop. A missing path is an error.
func TestTransferPathFromProtobufRefusesAMissingPath(t *testing.T) {
	if _, err := TransferPathFromProtobuf(nil); err == nil {
		t.Fatal("a missing path was accepted")
	}
}
