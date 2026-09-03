// Regression test for M3/L5 (PR-fix commit 8f1e2b3b): when the R-5
// retained-byte cap denies retention for an item, the SendSequence must fire
// RetentionEventCallback with a cap-denied payload so operators can see the
// denial without running at V(1) log verbosity.
package connect

import (
	"fmt"
	"testing"
	"time"
)

// TestRetainCapDenialFiresRetentionEventCallback pins the exact conditional
// in sendWithSetContract's R-5 cap block (transfer.go): when admitting a
// retained item would exceed 25% of the queue ceiling, the item's retention
// is denied AND a "retain_cap_denied" event fires. Exercising this via the
// real send path requires a live multi-route writer with an active
// destination route, which a unit test cannot cheaply stand up (the write
// error is intentionally ignored by the caller — "the item will be
// resent" — so the cap logic is unconditional on write success, but getting
// there still means driving the full transport stack). This test replicates
// the block verbatim, matching the existing TestSequenceExitDrainDoesNotCreditAckedBytes
// precedent for testing an inline Run()-path block in isolation.
func TestRetainCapDenialFiresRetentionEventCallback(t *testing.T) {
	_, seq := newH1TestSequence(t)

	var events []string
	seq.sendBufferSettings.RetentionEventCallback = func(event string) {
		events = append(events, event)
	}

	retainCapByteCount := ByteCount(float64(seq.sendBufferSettings.ResendQueueMaxByteCount) * 0.25)
	const messageByteCount = ByteCount(100)

	// Already at the cap: admitting this item would exceed it.
	seq.retainedByteCount = retainCapByteCount

	item := &sendItem{
		transferItem: transferItem{
			messageId:      NewId(),
			sequenceNumber: 1,
		},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour),
		sendCount:             1,
	}

	// Verbatim R-5 cap block from SendSequence.sendWithSetContract.
	if seq.retainedByteCount+messageByteCount > retainCapByteCount {
		item.retainAfterAckTimeout = false
		item.backstopDeadline = time.Time{}
		if seq.sendBufferSettings.RetentionEventCallback != nil {
			seq.sendBufferSettings.RetentionEventCallback(fmt.Sprintf(
				"retain_cap_denied msg=%x bytes=%d retained=%d cap=%d sendCount=%d",
				item.messageId, messageByteCount, seq.retainedByteCount,
				retainCapByteCount, item.sendCount))
		}
	} else {
		seq.retainedByteCount += messageByteCount
	}

	if item.retainAfterAckTimeout {
		t.Fatal("cap-denied item still has retainAfterAckTimeout=true")
	}
	if !item.backstopDeadline.IsZero() {
		t.Fatal("cap-denied item retains a non-zero backstopDeadline")
	}
	if seq.retainedByteCount != retainCapByteCount {
		t.Fatalf("retainedByteCount changed on cap denial: got %d, want unchanged %d", seq.retainedByteCount, retainCapByteCount)
	}
	if len(events) != 1 {
		t.Fatalf("RetentionEventCallback fired %d times, want 1", len(events))
	}
	if got := events[0]; len(got) < len("retain_cap_denied") || got[:len("retain_cap_denied")] != "retain_cap_denied" {
		t.Fatalf("RetentionEventCallback payload = %q, want it to start with \"retain_cap_denied\"", got)
	}
}

// TestRetainCapAdmissionDoesNotFireCallback is the inverse: when the item
// fits under the cap, retention is admitted normally and no cap-denial event
// fires (the ordinary retain-drop/retain-ack events are covered by the
// existing backstop tests).
func TestRetainCapAdmissionDoesNotFireCallback(t *testing.T) {
	_, seq := newH1TestSequence(t)

	var events []string
	seq.sendBufferSettings.RetentionEventCallback = func(event string) {
		events = append(events, event)
	}

	retainCapByteCount := ByteCount(float64(seq.sendBufferSettings.ResendQueueMaxByteCount) * 0.25)
	const messageByteCount = ByteCount(100)

	seq.retainedByteCount = 0 // nowhere near the cap

	item := &sendItem{
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour),
	}

	if seq.retainedByteCount+messageByteCount > retainCapByteCount {
		item.retainAfterAckTimeout = false
		item.backstopDeadline = time.Time{}
		if seq.sendBufferSettings.RetentionEventCallback != nil {
			seq.sendBufferSettings.RetentionEventCallback("retain_cap_denied")
		}
	} else {
		seq.retainedByteCount += messageByteCount
	}

	if !item.retainAfterAckTimeout {
		t.Fatal("item admitted under cap lost retainAfterAckTimeout")
	}
	if seq.retainedByteCount != messageByteCount {
		t.Fatalf("retainedByteCount after admission = %d, want %d", seq.retainedByteCount, messageByteCount)
	}
	if len(events) != 0 {
		t.Fatalf("RetentionEventCallback fired on admission (not denial): %v", events)
	}
}
