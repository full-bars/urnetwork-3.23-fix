package connect

// The retained-accounting flags are opt-in. Everything here is about the
// boundary: a default construction must take exactly the accounting path it
// took before the flags existed, and a parented budget must force the flag on
// so a composed pool can never fall back to an uncharged per-flow floor
// (transfer.go:6918 and :14635 in the pin).

import (
	"context"
	"testing"
)

func newRetainedAccountingTestClient(t *testing.T) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.Log = NewNoopLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
}

// Every flag is false in a default construction, and the third one has no
// consumer in this tree yet.
func TestRetainedByteAccountingFlagsDefaultOff(t *testing.T) {
	send := DefaultSendBufferSettings()
	AssertEqual(t, send.ResendQueueRetainedByteAccounting, false)
	AssertEqual(t, send.ResendQueueBudget == nil, true)

	receive := DefaultReceiveBufferSettings()
	AssertEqual(t, receive.ReceiveQueueRetainedByteAccounting, false)
	AssertEqual(t, receive.PackQueueRetainedByteAccounting, false)
	AssertEqual(t, receive.ReceiveQueueBudget == nil, true)

	// a zero value is the same as the defaults here
	AssertEqual(t, SendBufferSettings{}.ResendQueueRetainedByteAccounting, false)
	AssertEqual(t, ReceiveBufferSettings{}.ReceiveQueueRetainedByteAccounting, false)
	AssertEqual(t, ReceiveBufferSettings{}.PackQueueRetainedByteAccounting, false)
}

// Identical traffic on a default receive queue and on one that opts in. The
// items and both totals are the same; only who reserves against the pool
// differs, which is the whole point of the flag.
func TestDefaultReceiveQueueAccountingPathIsUnchanged(t *testing.T) {
	client := newRetainedAccountingTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	newSequence := func(retained bool, budget *TransferMemoryBudget) *ReceiveSequence {
		settings := DefaultReceiveBufferSettings()
		settings.ReceiveQueueMaxByteCount = kib(64)
		settings.ReceiveQueueMinByteCount = kib(8)
		settings.ReceiveQueueBudget = budget
		settings.ReceiveQueueRetainedByteAccounting = retained
		return NewReceiveSequence(ctx, client, TransferPath{}, NewId(), sequenceTlsRoleClient, false, settings)
	}

	defaultBudget := NewTransferMemoryBudget(kib(64))
	retainedBudget := NewTransferMemoryBudget(kib(64))
	defaultSeq := newSequence(false, defaultBudget)
	retainedSeq := newSequence(true, retainedBudget)

	addTraffic := func(seq *ReceiveSequence) {
		for i := 1; i <= 4; i += 1 {
			seq.receiveQueue.Add(&receiveItem{transferItem: transferItem{
				messageId:        NewId(),
				messageByteCount: kib(4),
				sequenceNumber:   uint64(i),
			}})
		}
	}
	addTraffic(defaultSeq)
	addTraffic(retainedSeq)

	// the same items, the same logical and retained totals
	AssertEqual(t, defaultSeq.receiveQueue.byteCount, retainedSeq.receiveQueue.byteCount)
	AssertEqual(t, defaultSeq.receiveQueue.queueByteCount, retainedSeq.receiveQueue.queueByteCount)
	AssertEqual(t, defaultSeq.receiveQueue.byteCount, kib(16))

	// the default path is the pre-flag one: the queue reserves above its floor
	AssertEqual(t, defaultSeq.receiveQueue.lifetimeBudget, false)
	AssertEqual(t, defaultSeq.receiveQueue.minByteCount, kib(8))
	AssertEqual(t, defaultSeq.receiveQueue.borrowedByteCount, kib(8))
	AssertEqual(t, defaultBudget.UsedByteCount(), kib(8))
	AssertEqual(t, defaultSeq.receiveQueue.borrowedByteCount, max(0, defaultSeq.receiveQueue.byteCount-kib(8)))

	// opting in leaves the reservation to the item, so the queue holds the
	// floor at zero and reserves nothing itself
	AssertEqual(t, retainedSeq.receiveQueue.lifetimeBudget, true)
	AssertEqual(t, retainedSeq.receiveQueue.minByteCount, ByteCount(0))
	AssertEqual(t, retainedBudget.UsedByteCount(), ByteCount(0))
	AssertEqual(t, retainedSeq.receiveQueue.borrowedByteCount, kib(16))

	// tearing both down returns the pools to their starting counts
	defaultSeq.receiveQueue.Clear()
	retainedSeq.receiveQueue.Clear()
	AssertEqual(t, defaultBudget.UsedByteCount(), ByteCount(0))
	AssertEqual(t, retainedBudget.UsedByteCount(), ByteCount(0))
	reserved, released := defaultBudget.Counts()
	AssertEqual(t, reserved, released)
	reserved, released = retainedBudget.Counts()
	AssertEqual(t, reserved, released)
}

// A parented budget selects exact item ownership on its own. The sequence
// records the forced flag on its own settings copy and leaves the caller's
// struct alone.
func TestParentedBudgetForcesRetainedByteAccounting(t *testing.T) {
	client := newRetainedAccountingTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	root := NewTransferMemoryBudget(kib(128))

	t.Run("send", func(t *testing.T) {
		child := NewTransferMemoryBudgetWithParent(kib(32), root)
		settings := DefaultSendBufferSettings()
		settings.ResendQueueBudget = child
		settings.ResendQueueRetainedByteAccounting = false
		sendBuffer := NewSendBuffer(ctx, client, settings)

		seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, settings)
		AssertEqual(t, seq.sendBufferSettings == settings, false)
		AssertEqual(t, seq.sendBufferSettings.ResendQueueRetainedByteAccounting, true)
		AssertEqual(t, settings.ResendQueueRetainedByteAccounting, false)
		AssertEqual(t, seq.resendQueue.lifetimeBudget, true)
		AssertEqual(t, seq.resendQueue.minByteCount, ByteCount(0))
		AssertEqual(t, seq.resendQueue.Budget(), child)
	})

	t.Run("send unparented budget keeps the caller's settings", func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.ResendQueueBudget = NewTransferMemoryBudget(kib(32))
		settings.ResendQueueRetainedByteAccounting = false
		sendBuffer := NewSendBuffer(ctx, client, settings)

		seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, settings)
		AssertEqual(t, seq.sendBufferSettings == settings, true)
		AssertEqual(t, seq.sendBufferSettings.ResendQueueRetainedByteAccounting, false)
		AssertEqual(t, seq.resendQueue.lifetimeBudget, false)
		AssertEqual(t, seq.resendQueue.minByteCount, settings.ResendQueueMinByteCount)
	})

	t.Run("receive", func(t *testing.T) {
		child := NewTransferMemoryBudgetWithParent(kib(32), root)
		settings := DefaultReceiveBufferSettings()
		settings.ReceiveQueueBudget = child
		settings.ReceiveQueueRetainedByteAccounting = false

		seq := NewReceiveSequence(ctx, client, TransferPath{}, NewId(), sequenceTlsRoleClient, false, settings)
		AssertEqual(t, seq.receiveBufferSettings == settings, false)
		AssertEqual(t, seq.receiveBufferSettings.ReceiveQueueRetainedByteAccounting, true)
		AssertEqual(t, settings.ReceiveQueueRetainedByteAccounting, false)
		AssertEqual(t, seq.receiveQueue.lifetimeBudget, true)
		AssertEqual(t, seq.receiveQueue.minByteCount, ByteCount(0))
		AssertEqual(t, seq.receiveQueue.Budget(), child)
	})

	t.Run("receive unparented budget keeps the caller's settings", func(t *testing.T) {
		settings := DefaultReceiveBufferSettings()
		settings.ReceiveQueueBudget = NewTransferMemoryBudget(kib(32))
		settings.ReceiveQueueRetainedByteAccounting = false

		seq := NewReceiveSequence(ctx, client, TransferPath{}, NewId(), sequenceTlsRoleClient, false, settings)
		AssertEqual(t, seq.receiveBufferSettings == settings, true)
		AssertEqual(t, seq.receiveBufferSettings.ReceiveQueueRetainedByteAccounting, false)
		AssertEqual(t, seq.receiveQueue.lifetimeBudget, false)
		AssertEqual(t, seq.receiveQueue.minByteCount, settings.ReceiveQueueMinByteCount)
	})

	// the gateway share was never spent: a parented child reserves nothing by
	// existing, it only changes who is allowed to keep what it charges
	AssertEqual(t, root.UsedByteCount(), ByteCount(0))
}
