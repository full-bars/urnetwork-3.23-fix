package connect

// The transfer queue keeps two totals: the protocol/message total that gates a
// per-sequence window, and the retained-allocation total that gates the shared
// budget (MEMSTEADY.md:623-654). These tests pin the parts a port can silently
// get wrong: an item type that inherited the wrong `QueueByteCount`, a queue
// that charged one total twice, and the floor/lifetime bookkeeping that decides
// whether the queue or the item reserves.

import (
	"testing"
	"time"
)

// Every item type answers QueueByteCount with its own message size. The base
// transferItem answers with its raw messageByteCount, receiveItem inherits
// that, and sendItem must answer with the encoded frame length because
// transferItem.QueueByteCount cannot see the embedded type's override.
func TestTransferQueueByteCountMatchesMessageByteCountPerItemType(t *testing.T) {
	base := &transferItem{messageId: NewId(), messageByteCount: 1500, sequenceNumber: 1}
	AssertEqual(t, base.QueueByteCount(), base.MessageByteCount())

	// a charge carried by the item itself overrides the base size
	retained := &transferItem{messageId: NewId(), messageByteCount: 1500, queueByteCount: 4096, sequenceNumber: 2}
	AssertEqual(t, retained.QueueByteCount(), ByteCount(4096))
	// the logical total is untouched by the retained charge
	AssertEqual(t, retained.MessageByteCount(), ByteCount(1500))

	received := &receiveItem{transferItem: transferItem{messageId: NewId(), messageByteCount: 1500, sequenceNumber: 3}}
	AssertEqual(t, received.QueueByteCount(), received.MessageByteCount())

	// the send queue charges the encoded frame, not the application message
	sent := &sendItem{
		transferItem:       transferItem{messageId: NewId(), messageByteCount: 40, sequenceNumber: 4},
		transferFrameBytes: make([]byte, 1500),
	}
	AssertEqual(t, sent.QueueByteCount(), ByteCount(1500))
	AssertEqual(t, sent.QueueByteCount(), sent.MessageByteCount())

	// the item pool reuses items; a cleared item must fall back to its message
	// size rather than keep the previous owner's retained charge
	sent.messageByteCount = 32
	sent.transferFrameBytes = nil
	AssertEqual(t, sent.QueueByteCount(), ByteCount(32))
}

// The queue tracks the logical total for the per-sequence window and the
// retained total for the shared budget, from the same add/remove path.
func TestTransferQueueTracksLogicalAndRetainedTotals(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(64))
	queue := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		if a.sequenceNumber < b.sequenceNumber {
			return -1
		} else if b.sequenceNumber < a.sequenceNumber {
			return 1
		}
		return 0
	})
	queue.setBudget(budget, 0)

	// a 1,500-byte frame that retains a 4-KiB backing class
	item := &transferItem{
		messageId:        NewId(),
		messageByteCount: 1500,
		queueByteCount:   kib(4),
		sequenceNumber:   1,
	}
	queue.Add(item)

	size, logicalByteCount := queue.QueueSize()
	AssertEqual(t, size, 1)
	AssertEqual(t, logicalByteCount, ByteCount(1500))
	AssertEqual(t, queue.queueByteCount, kib(4))
	AssertEqual(t, budget.UsedByteCount(), kib(4))

	// the logical window admits on the payload, the pool admits on retained
	AssertEqual(t, queue.CanAddWithQueueByteCount(1500, kib(4), kib(3)), true)
	AssertEqual(t, queue.CanAddWithQueueByteCount(kib(8), kib(8), kib(3)), false)

	// removal releases both totals
	queue.RemoveByMessageId(item.messageId)
	AssertEqual(t, queue.byteCount, ByteCount(0))
	AssertEqual(t, queue.queueByteCount, ByteCount(0))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

// ObtainableByteCount is the floor plus what the queue already borrowed plus
// what the pool has left, and is zero when there is no pool to borrow from.
func TestTransferQueueObtainableByteCount(t *testing.T) {
	queue := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		return 0
	})
	AssertEqual(t, queue.Budget() == nil, true)
	AssertEqual(t, queue.ObtainableByteCount(), ByteCount(0))

	budget := NewTransferMemoryBudget(kib(8))
	queue.setBudget(budget, kib(2))
	AssertEqual(t, queue.Budget(), budget)
	// 2 KiB floor + nothing borrowed + 8 KiB free
	AssertEqual(t, queue.ObtainableByteCount(), kib(10))

	queue.Add(&transferItem{messageId: NewId(), messageByteCount: kib(4), sequenceNumber: 1})
	AssertEqual(t, queue.borrowedByteCount, kib(2))
	// 2 KiB floor + 2 KiB borrowed + 6 KiB free
	AssertEqual(t, queue.ObtainableByteCount(), kib(10))

	// another claim on the pool shrinks what this queue could obtain
	budget.Reserve(kib(4))
	// 2 KiB floor + 2 KiB borrowed + 2 KiB free
	AssertEqual(t, queue.ObtainableByteCount(), kib(6))
}

// The fork's send and receive queues charge no retained class, so the two
// totals stay equal for them and the borrowed invariant is exactly the pre-port
// one: borrowed = max(0, total - floor). This is the no-default-change pin for
// the two-total accounting.
func TestTransferQueueDefaultQueuesKeepOneTotal(t *testing.T) {
	t.Run("resend", func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(16))
		queue := newResendQueue(budget, kib(4))
		for i := 1; i <= 6; i += 1 {
			queue.Add(&sendItem{
				transferItem:       transferItem{messageId: NewId(), messageByteCount: kib(1), sequenceNumber: uint64(i)},
				transferFrameBytes: make([]byte, 1500),
				resendTime:         time.Time{},
			})
			AssertEqual(t, queue.queueByteCount, queue.byteCount)
			AssertEqual(t, queue.borrowedByteCount, max(0, queue.byteCount-kib(4)))
			AssertEqual(t, budget.UsedByteCount(), queue.borrowedByteCount)
		}
		for 0 < queue.Len() {
			queue.RemoveFirst()
			AssertEqual(t, queue.queueByteCount, queue.byteCount)
			AssertEqual(t, queue.borrowedByteCount, max(0, queue.byteCount-kib(4)))
			AssertEqual(t, budget.UsedByteCount(), queue.borrowedByteCount)
		}
		queue.Clear()
		AssertEqual(t, queue.queueByteCount, ByteCount(0))
		reserved, released := budget.Counts()
		AssertEqual(t, reserved, released)
	})

	t.Run("receive", func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(16))
		queue := newReceiveQueue(budget, kib(4))
		for i := 1; i <= 6; i += 1 {
			queue.Add(&receiveItem{transferItem: transferItem{messageId: NewId(), messageByteCount: kib(1), sequenceNumber: uint64(i)}})
			AssertEqual(t, queue.queueByteCount, queue.byteCount)
			AssertEqual(t, queue.borrowedByteCount, max(0, queue.byteCount-kib(4)))
			AssertEqual(t, budget.UsedByteCount(), queue.borrowedByteCount)
		}
		for 0 < queue.Len() {
			queue.RemoveBySequenceNumber(queue.PeekFirst().SequenceNumber())
			AssertEqual(t, queue.queueByteCount, queue.byteCount)
			AssertEqual(t, queue.borrowedByteCount, max(0, queue.byteCount-kib(4)))
			AssertEqual(t, budget.UsedByteCount(), queue.borrowedByteCount)
		}
		queue.Clear()
		AssertEqual(t, queue.queueByteCount, ByteCount(0))
		reserved, released := budget.Counts()
		AssertEqual(t, reserved, released)
	})
}

func TestTransferQueueBudgetFloorRegistration(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(8))
	queue := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		return 0
	})
	queue.setBudget(budget, kib(2))
	AssertEqual(t, queue.floorRegistered, true)
	// the pool less the floors guaranteed to the other queues
	AssertEqual(t, budget.LendableByteCount(kib(2)), kib(8))

	second := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		return 0
	})
	second.setBudget(budget, kib(4))
	AssertEqual(t, budget.LendableByteCount(kib(4)), kib(6))

	second.Clear()
	AssertEqual(t, second.floorRegistered, false)
	AssertEqual(t, budget.LendableByteCount(kib(2)), kib(8))

	// re-attaching a floor replaces it rather than double counting
	queue.setBudget(budget, kib(1))
	AssertEqual(t, queue.minByteCount, kib(1))
	AssertEqual(t, budget.LendableByteCount(kib(1)), kib(8))
	queue.Clear()
	AssertEqual(t, budget.LendableByteCount(0), kib(8))
}

// With a lifetime budget the item owns exact admission, so the queue stops
// reserving per byte and stops claiming a floor. The one-item liveness
// exemption is deliberately dropped too: an empty queue is not permission to
// overdraw a pool someone else has already spent.
func TestTransferQueueLifetimeBudgetStopsQueueReservation(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(8))
	queue := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		return 0
	})
	queue.setBudget(budget, kib(4))
	AssertEqual(t, budget.LendableByteCount(kib(4)), kib(8))

	queue.setLifetimeBudget()
	AssertEqual(t, queue.lifetimeBudget, true)
	AssertEqual(t, queue.minByteCount, ByteCount(0))
	AssertEqual(t, queue.floorRegistered, true)

	// the queue no longer reserves: the item's own admission is the charge
	queue.Add(&transferItem{messageId: NewId(), messageByteCount: kib(6), sequenceNumber: 1})
	AssertEqual(t, queue.borrowedByteCount, kib(6))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	// with the pool spent, an empty lifetime queue refuses where a
	// floor-registered queue would still admit its first item
	budget.Reserve(kib(8))
	empty := newTransferQueue[*transferItem](func(a *transferItem, b *transferItem) int {
		return 0
	})
	empty.setBudget(budget, 0)
	AssertEqual(t, empty.CanAdd(kib(1), kib(64)), true)
	empty.setLifetimeBudget()
	AssertEqual(t, empty.CanAdd(kib(1), kib(64)), false)

	budget.Release(kib(8))
	empty.setLifetimeBudget()
	AssertEqual(t, empty.CanAdd(kib(1), kib(64)), true)

	// Clear releases the borrowed bookkeeping and the floor
	queue.Clear()
	AssertEqual(t, queue.borrowedByteCount, ByteCount(0))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
}
