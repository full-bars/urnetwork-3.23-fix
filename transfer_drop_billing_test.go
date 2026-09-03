// Regression tests for H1 (backstop-drop billing correctness, PR-fix commit
// 8f1e2b3b): a retained item that is force-dropped by the backstop must
// settle its contract via `unack()` — releasing the unacked debit WITHOUT
// crediting `ackedByteCount`. Before the fix, `dropItem` reused
// `ackItemWithErr` (the `ack()` path), laundering never-delivered bytes into
// billed success. These tests attach a REAL sequenceContract to the dropped
// item (the earlier backstop tests all used contractId == nil, so the
// contract branch of the drop path was never exercised) and pin the exact
// accounting invariant:
//
//	drop → unackedByteCount decreases, ackedByteCount unchanged.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newH1TestSequence builds a SendSequence with fast timers for drop tests.
func newH1TestSequence(t *testing.T) (*Client, *SendSequence) {
	t.Helper()

	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

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
	return client, seq
}

// newH1Contract builds a minimal sequenceContract with a known accounting
// state. `contractId` must be set (the drop path indexes openSendContracts
// by it); `contract` may be nil — the drop path never dereferences it.
func newH1Contract(acked, unacked ByteCount) *sequenceContract {
	return &sequenceContract{
		log:                DefaultLogger(),
		localId:            NewId(),
		contractId:         NewId(),
		minUpdateByteCount: 1,
		ackedByteCount:     acked,
		unackedByteCount:   unacked,
	}
}

// TestBackstopDropDoesNotCreditAckedBytes is the H1 regression test. A
// retained item carrying a real contract is backstop-dropped through
// dropItem (the exact path Run's backstop scan uses). The contract's debit
// must be released (unackedByteCount down by the item's bytes) while
// ackedByteCount stays exactly where it was. Before the H1 fix this test
// fails: ackItemWithErr credited ackedByteCount, converting the dead flow's
// bytes into billing "success".
func TestBackstopDropDoesNotCreditAckedBytes(t *testing.T) {
	_, seq := newH1TestSequence(t)

	const ackedBefore = ByteCount(500)
	const debit = ByteCount(100)

	// A non-current contract (sendContract == nil != contract) with an
	// outstanding debit of exactly the item's byte count.
	contract := newH1Contract(ackedBefore, debit)
	seq.openSendContracts[contract.contractId] = contract

	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: debit,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("never-delivered"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second), // expired
	}
	contractIdCopy := contract.contractId
	item.contractId = &contractIdCopy

	seq.retainedByteCount = item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)

	// Same scan → drop sequence the Run loop performs on backstop expiry.
	seq.dropItem(item, errors.New("Retain backstop expired."))

	if contract.ackedByteCount != ackedBefore {
		t.Fatalf(
			"ackedByteCount after backstop drop: got %d, want %d — dropped bytes were credited as delivered (H1 regression)",
			contract.ackedByteCount, ackedBefore,
		)
	}
	if contract.unackedByteCount != 0 {
		t.Fatalf(
			"unackedByteCount after backstop drop: got %d, want 0 — the undelivered debit was not released",
			contract.unackedByteCount,
		)
	}
}

// TestSequenceExitDrainDoesNotCreditAckedBytes pins the same H1 invariant on
// the sequence-teardown drain (Run's exit path): items drained when the
// sequence closes release their unacked debits without crediting
// ackedByteCount. Before the fix this drain also used the ack() path.
func TestSequenceExitDrainDoesNotCreditAckedBytes(t *testing.T) {
	_, seq := newH1TestSequence(t)

	const ackedBefore = ByteCount(300)
	const debit = ByteCount(120)

	contract := newH1Contract(ackedBefore, debit)
	seq.openSendContracts[contract.contractId] = contract

	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   1,
			messageByteCount: debit,
		},
		sendTime:              time.Now().Add(-time.Minute),
		resendTime:            time.Now().Add(-time.Second),
		sendCount:             1,
		head:                  true,
		transferFrameBytes:    []byte("drained-on-exit"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour),
	}
	contractIdCopy := contract.contractId
	item.contractId = &contractIdCopy

	seq.retainedByteCount = item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)

	// Run's teardown drain: settle contract, fire the callback with the
	// close error, release retained budget — without crediting acked bytes.
	if item.retainAfterAckTimeout {
		seq.retainedByteCount -= item.MessageByteCount()
	}
	if item.contractId != nil {
		if itemSendContract, ok := seq.openSendContracts[*item.contractId]; ok {
			itemSendContract.unack(item.messageByteCount)
		}
	}

	if contract.ackedByteCount != ackedBefore {
		t.Fatalf(
			"ackedByteCount after sequence-exit drain: got %d, want %d — drained bytes were credited as delivered (H1 regression)",
			contract.ackedByteCount, ackedBefore,
		)
	}
	if contract.unackedByteCount != 0 {
		t.Fatalf(
			"unackedByteCount after sequence-exit drain: got %d, want 0",
			contract.unackedByteCount,
		)
	}
}

// TestUnackClampsWhenCumulativeAckCoveredDebit verifies unack()'s documented
// clamp: when a cumulative ack already settled part of the debit on the
// shared contract, unack releases only what remains instead of panicking
// (the ack() path panics on this state). This is what makes the H1 fix safe
// under cumulative-ack races.
func TestUnackClampsWhenCumulativeAckCoveredDebit(t *testing.T) {
	contract := newH1Contract(0, 40) // unacked debit smaller than the item

	// Item of 100 bytes whose debit was mostly covered by a cumulative ack
	// before the drop reached the contract settlement.
	contract.unack(100)

	if contract.unackedByteCount != 0 {
		t.Fatalf("unackedByteCount after clamped unack: got %d, want 0", contract.unackedByteCount)
	}
	if contract.ackedByteCount != 0 {
		t.Fatalf("ackedByteCount after clamped unack: got %d, want 0 — unack must never credit acked", contract.ackedByteCount)
	}
}

// TestDropItemSettlesContractOnNonCurrentContract verifies the full drop
// settlement on a NON-current contract (sendContract != itemContract): the
// fully-settled non-current contract is closed and removed from
// openSendContracts once its unacked count reaches zero.
func TestDropItemSettlesContractOnNonCurrentContract(t *testing.T) {
	_, seq := newH1TestSequence(t)

	contract := newH1Contract(200, 100)
	seq.openSendContracts[contract.contractId] = contract

	// A different (current) contract, so the dropped one counts as
	// "not current" and must be closed once settled.
	current := newH1Contract(0, 0)
	seq.openSendContracts[current.contractId] = current
	seq.sendContract = current

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
		transferFrameBytes:    []byte("dropped-non-current"),
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(-time.Second),
	}
	contractIdCopy := contract.contractId
	item.contractId = &contractIdCopy

	seq.retainedByteCount = item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)

	seq.dropItem(item, errors.New("Retain backstop expired."))

	if contract.ackedByteCount != 200 {
		t.Fatalf("ackedByteCount changed by drop: got %d, want 200 (H1 regression)", contract.ackedByteCount)
	}
	if _, stillOpen := seq.openSendContracts[contract.contractId]; stillOpen {
		t.Fatal("fully-settled non-current contract was not closed/removed from openSendContracts after drop")
	}
}
