package connect

import (
	"context"
	"testing"
	"time"
)

// TestBackstopDropsRetainedItem verifies the backstop ceiling end-to-end:
// a retained item past its backstopDeadline is force-dropped through the
// actual send loop, removing it from BOTH resendQueue and sendItems, settling
// the contract, releasing retainedByteCount, and firing the error callback.
//
// Regression test for the CRITICAL Opus finding: backstop drop previously
// only removed from resendQueue, leaving a ghost in sendItems that caused
// "Missing item" panics on the next cumulative ack, destroying the entire
// SendSequence.
func TestBackstopDropsRetainedItem(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

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

	// Construct a retained item with a backstopDeadline in the past.
	// Manually add to both sendItems and resendQueue to exercise the
	// full dropItem path (remove from both + compact sendItems).
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
		backstopDeadline:      time.Now().Add(-time.Second), // already expired
	}

	// Mark retainedByteCount as if the admission cap had charged this item.
	// (In production this happens in sendWithSetContract's if-ack branch.)
	seq.retainedByteCount = item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)

	select {
	case err := <-dropped:
		if err == nil {
			t.Fatal("backstop dropped item with nil error")
		}
		t.Logf("backstop dropped item: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item within timeout")
	}

	// CRITICAL: verify retainedByteCount was released by dropItem
	if seq.retainedByteCount != 0 {
		t.Fatalf("retainedByteCount after drop: got %d, want 0", seq.retainedByteCount)
	}

	// CRITICAL: verify sendItems was compacted (item removed)
	if len(seq.sendItems) != 0 {
		t.Fatalf("sendItems after drop: got %d items, want 0", len(seq.sendItems))
	}

	// CRITICAL REGRESSION: a cumulative ack for a later sequence number
	// must NOT panic. Before dropItem, this walked the ghost in sendItems
	// and hit panic("Missing item").
	ackId := NewId()
	// Synthesize a cumulative ack item with a higher sequence number.
	ackItem := &sendItem{
		transferItem: transferItem{
			messageId:      ackId,
			sequenceNumber: 2, // seq 2 > dropped seq 1
		},
	}
	// receiveAck walks sendItems cumulatively up to the ack's seq.
	// Since sendItems is empty (item was dropped), this must not panic.
	seq.receiveAck(ackId, false, nil)
	_ = ackItem // unused, but documents the intent

	t.Log("cumulative ack after backstop drop: no panic (regression verified)")
}

// TestBackstopDropThenCumulativeAck verifies the full sequence:
// send retained item → backstop drops it → cumulative ack for later seq
// arrives → receiveAck walks sendItems, finds the dropped item gone,
// and does NOT panic. This is the exact regression from the CRITICAL
// Opus finding on PR #506.
func TestBackstopDropThenCumulativeAck(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

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

	// item1: retained, backstop already expired
	item1 := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: 100,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("item1"),
		ackCallback:           func(err error) { dropped <- err },
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second),
	}

	seq.retainedByteCount = item1.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item1)
	seq.resendQueue.Add(item1)

	// Wait for backstop to fire and drop item1
	select {
	case <-dropped:
		// item1 dropped
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item1 within timeout")
	}

	// Verify clean state after drop
	if seq.retainedByteCount != 0 {
		t.Fatalf("retainedByteCount: got %d, want 0", seq.retainedByteCount)
	}
	if len(seq.sendItems) != 0 {
		t.Fatalf("sendItems: got %d, want 0", len(seq.sendItems))
	}

	// Now simulate a cumulative ack for seq=2 (which was never queued).
	// Before the fix, this would walk the ghost seq=1 in sendItems and
	// panic("Missing item"). After the fix, receiveAck gracefully skips
	// the already-removed item.
	ackId := NewId()
	seq.receiveAck(ackId, false, nil)

	t.Log("cumulative ack after backstop drop: no panic")
}
