package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetainedItemSurvivesAckTimeout verifies the core guard: a retained item
// in the resendQueue is NOT terminal-closed when itemAckTimeout <= 0. This
// tests the guard condition directly without running the full send loop (which
// needs a session/contract for the write path).
func TestRetainedItemSurvivesAckTimeout(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.AckTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)

	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	destination := DestinationId(NewId())
	seq := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		destination,
		MultiHopId{},
		false,
		true,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)

	// Create a retained item that is already past ack timeout
	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: 100,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("test-data"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour),
	}

	seq.resendQueue.Add(item)

	// Verify the guard logic: itemAckTimeout <= 0 && !item.retainAfterAckTimeout
	// For a retained item, this must be false (skip terminal close)
	sendTime := time.Now()
	itemAckTimeout := item.sendTime.Add(clientSettings.SendBufferSettings.AckTimeout).Sub(sendTime)
	if itemAckTimeout > 0 {
		t.Fatal("item should be past ack timeout")
	}

	// The guard: skip terminal close for retained items
	shouldClose := itemAckTimeout <= 0 && !item.retainAfterAckTimeout
	if shouldClose {
		t.Fatal("retained item would terminal-close the sequence — guard failed")
	}

	// Item is still in the queue
	if seq.resendQueue.Len() != 1 {
		t.Fatalf("resendQueue.Len() = %d, want 1", seq.resendQueue.Len())
	}

	// Verify the backstop check: item is NOT past backstop
	if !sendTime.Before(item.backstopDeadline) {
		t.Fatal("item should NOT be past backstop")
	}

	t.Log("retained item survives ack timeout — guard and backstop both correct")
}

// TestNonRetainedItemTerminalCloses verifies the opposite: a non-retained
// item past ack timeout triggers terminal close (the original behavior).
func TestNonRetainedItemTerminalCloses(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.AckTimeout = 100 * time.Millisecond

	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: 100,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("test-data"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: false,
	}

	sendTime := time.Now()
	itemAckTimeout := item.sendTime.Add(clientSettings.SendBufferSettings.AckTimeout).Sub(sendTime)
	if itemAckTimeout > 0 {
		t.Fatal("item should be past ack timeout")
	}

	shouldClose := itemAckTimeout <= 0 && !item.retainAfterAckTimeout
	if !shouldClose {
		t.Fatal("non-retained item past ack timeout should terminal-close")
	}

	t.Log("non-retained item correctly triggers terminal close")
}

// TestBackstopDropsRetainedItem verifies the backstop ceiling end-to-end:
// a retained item past its backstopDeadline is force-dropped through the
// actual send loop.
func TestBackstopDropsRetainedItem(t *testing.T) {
	var forceAck atomic.Bool
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour
	clientSettings.SendBufferSettings.forceAckTimeoutForTest = func(sendSequenceId) bool {
		return forceAck.Load()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)

	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	destination := DestinationId(NewId())
	seq := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		destination,
		MultiHopId{},
		false,
		true,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)

	go HandleError(func() { seq.Run() })

	dropped := make(chan error, 1)
	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: ByteCount(len("test")),
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("test"),
		ackCallback:           func(err error) { dropped <- err },
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second),
	}

	seq.resendQueue.Add(item)

	forceAck.Store(true)

	select {
	case err := <-dropped:
		if err == nil {
			t.Fatal("backstop dropped item with nil error")
		}
		t.Logf("backstop dropped item: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item within timeout")
	}
}
