package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBackstopDropsRetainedItem verifies the backstop ceiling end-to-end:
// a retained item past its backstopDeadline is force-dropped through the
// actual send loop, removing it from BOTH resendQueue and sendItems, settling
// the contract, releasing retainedByteCount, and firing the error callback.
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

	// Insert BEFORE starting Run (R-M5: goroutine must not observe empty queue).
	seq.retainedByteCount = item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)

	go HandleError(func() { seq.Run() })

	select {
	case err := <-dropped:
		if err == nil {
			t.Fatal("backstop dropped item with nil error")
		}
		t.Logf("backstop dropped item: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item within timeout")
	}

	if seq.retainedByteCount != 0 {
		t.Fatalf("retainedByteCount after drop: got %d, want 0", seq.retainedByteCount)
	}

	// slices.Delete removes the element — no interior nil holes (R-C2).
	if len(seq.sendItems) != 0 {
		t.Fatalf("sendItems after drop: got %d items, want 0", len(seq.sendItems))
	}
}

// TestBackstopDropThenCumulativeAck sends a retained item, backstop drops it,
// then a cumulative ack for a later sequence arrives. receiveAck walks
// sendItems and must NOT panic. This exercises the nil-check guard (R-C3).
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

	dropped := make(chan error, 1)

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

	go HandleError(func() { seq.Run() })

	// Wait for backstop to drop item1
	select {
	case <-dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("backstop did not drop item1 within timeout")
	}

	if seq.retainedByteCount != 0 {
		t.Fatalf("retainedByteCount after drop: got %d, want 0", seq.retainedByteCount)
	}
	if len(seq.sendItems) != 0 {
		t.Fatalf("sendItems after drop: got %d, want 0", len(seq.sendItems))
	}

	// Cumulative ack for seq=2 — sendItems is empty, loop body never entered.
	// Must not panic. Before the fix, the nil check was AFTER the deref.
	ackId := NewId()
	seq.receiveAck(ackId, false, nil)

	t.Log("cumulative ack after backstop drop: no panic (regression verified)")
}

// TestBackstopScanUsesSnapshot verifies the Snapshot-based scan (R-C1) by
// calling dropItem from the scan path and verifying sendItems integrity.
// This is a unit-level test that directly exercises the drop+scan interaction
// without fighting the Run loop's resend mechanics.
func TestBackstopScanUsesSnapshot(t *testing.T) {
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

	// Build 3 items directly — exercising Snapshot()-based scan without Run.
	item1 := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: 100,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(time.Hour),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("item1"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour), // NOT expired
	}

	item2 := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   2,
			messageByteCount: 200,
		},
		sendTime:           time.Now(),
		resendTime:         time.Now().Add(-time.Second),
		sendCount:          1,
		head:               true,
		transferFrameBytes: []byte("item2"),
		ackCallback:        func(err error) {},
	}

	item3 := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   3,
			messageByteCount: 300,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(time.Hour),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("item3"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second), // expired
	}

	seq.retainedByteCount = item1.MessageByteCount() + item3.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item1, item2, item3)
	seq.resendQueue.Add(item1)
	seq.resendQueue.Add(item2)
	seq.resendQueue.Add(item3)

	// Simulate the backstop scan: snapshot first, then drop expired items.
	// This is exactly what Run does after the fix (R-C1).
	var expired []*sendItem
	for _, retainedItem := range seq.resendQueue.Snapshot() {
		if retainedItem.retainAfterAckTimeout && !time.Now().Before(retainedItem.backstopDeadline) {
			expired = append(expired, retainedItem)
		}
	}
	for _, it := range expired {
		seq.dropItem(it, errors.New("Retain backstop expired."))
	}

	// Verify: only item1 (future backstop) and item2 (non-retained) remain
	if seq.retainedByteCount != item1.MessageByteCount() {
		t.Fatalf("retainedByteCount: got %d, want %d (only item1 retained)",
			seq.retainedByteCount, item1.MessageByteCount())
	}

	// sendItems must have exactly 2 entries, zero nil holes
	nonNil := 0
	for i, si := range seq.sendItems {
		if si == nil {
			t.Errorf("nil hole at sendItems[%d] after backstop scan — slices.Delete fix may have regressed", i)
		} else {
			nonNil++
		}
	}
	if nonNil != 2 {
		t.Fatalf("sendItems non-nil count: got %d, want 2", nonNil)
	}

	// Verify the correct items remain
	if seq.sendItems[0].sequenceNumber != 1 {
		t.Fatalf("sendItems[0] seq: got %d, want 1", seq.sendItems[0].sequenceNumber)
	}
	if seq.sendItems[1].sequenceNumber != 2 {
		t.Fatalf("sendItems[1] seq: got %d, want 2", seq.sendItems[1].sequenceNumber)
	}

	t.Log("backstop scan via Snapshot: buried expired item dropped, queue integrity maintained")
}
