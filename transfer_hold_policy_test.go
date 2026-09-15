// Regression tests for the receive hold policies (ported from upstream
// 609cb1b6 + 38d637c5, THROUGHPUTFIX §37.20): the committed-prefix policy
// acknowledges a held item only once it can no longer be evicted, so a held
// item that is later removed costs the sender a resend rather than a
// withdrawal of an acknowledgement it is already leasing. The core invariant:
// an acknowledged (committed) item is NEVER evicted.
//
// Scenario arithmetic (per-pack unit 1000, delivery point D, held items
// ascending, dense sequence numbers): the i-th held item is committed when
// missing_below(i) x maxFrame + heldBytes(0..i) <= capacity, where
// missing_below(i) = seq_i - D - i. Capacity 3500 (not a unit multiple) is
// chosen so a two-item hold under a two-pack gap overflows: 2*1000 + 2000 =
// 4000 > 3500 — that is what keeps an item tentative.
package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// newReceiveHoldTestSequence builds a ReceiveSequence with a tiny hold so a
// handful of out-of-order packs fill it.
func newReceiveHoldTestSequence(t *testing.T, policy ReceiveHoldPolicyKind, capacity ByteCount) (*Client, *ReceiveSequence) {
	t.Helper()

	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)

	settings := DefaultReceiveBufferSettings()
	settings.ReceiveHoldPolicy = policy
	settings.ReceiveQueueMaxByteCount = capacity
	settings.ReceiveQueueMinByteCount = 0
	seq := NewReceiveSequence(
		ctx,
		client,
		TransferPath{},
		NewId(),
		sequenceTlsRoleClient,
		false,
		settings,
	)
	return client, seq
}

// holdPack builds an out-of-order pack for the hold: head=false (a head pack
// would reset the delivery point), ack=true, so the item is held rather than
// delivered and the hold policy decides its acknowledgement.
func holdPack(sequenceNumber uint64, byteCount ByteCount) *ReceivePack {
	pack := &protocol.Pack{
		MessageId:      NewId().Bytes(),
		SequenceNumber: sequenceNumber,
		Nack:           false,
		Head:           false,
		Frames:         []*protocol.Frame{},
	}
	return &ReceivePack{
		Pack:               pack,
		MessageByteCount:   byteCount,
		TransferFrameBytes: nil,
	}
}

// The core invariant: under ReceiveHoldCommittedPrefix an acknowledged item
// is never evicted. Hold 2,3,4 (3000 bytes of a 3500 hold), then arrive with
// the missing seq 1: the full hold evicts newest-first, stops at the
// committed seq 2, and every item it removed was tentative.
func TestReceiveHoldCommittedPrefixNeverEvictsAcknowledged(t *testing.T) {
	const unit = ByteCount(1000)
	client, seq := newReceiveHoldTestSequence(t, ReceiveHoldCommittedPrefix, 3*unit+500)

	for _, n := range []uint64{2, 3, 4} {
		if ok, err := seq.receive(holdPack(n, unit)); err != nil || !ok {
			t.Fatalf("receive seq %d: ok=%v err=%v", n, ok, err)
		}
	}
	// seq 2 committed on admission (missing 2, 2*1000+1000 = 3000 <= 3500);
	// seq 3 and 4 stayed tentative (the newest check 2*1000+2000/3000 overflows).
	if got := client.ReceiveQueueCommitCount(); got != 1 {
		t.Fatalf("commits after the fill = %d, want 1 (only seq 2 is below the boundary)", got)
	}

	// the missing seq 1 arrives at a full hold
	if ok, err := seq.receive(holdPack(1, unit)); err != nil || !ok {
		t.Fatalf("receive seq 1: ok=%v err=%v", ok, err)
	}

	if got := client.ReceiveQueueEvictionCount(); got != 0 {
		t.Fatalf("committed evictions = %d, want 0 (acked items must never be discarded)", got)
	}
	if got := client.ReceiveQueueTentativeEvictionCount(); got != 1 {
		t.Fatalf("tentative evictions = %d, want 1 (seq 4 made room; one eviction clears 3500)", got)
	}
	found := false
	for _, held := range seq.receiveQueue.UnorderedItems(nil) {
		if held.sequenceNumber == 2 {
			found = true
			if !held.committed {
				t.Fatal("seq 2 lost its committed flag")
			}
		}
	}
	if !found {
		t.Fatal("seq 2 was evicted despite being committed")
	}
	// the head gap closed, so seq 1 commits and seq 2 stays committed; the
	// hold carries seq 1..3 (3000 bytes, under the 3500 capacity)
	if got := client.ReceiveQueueCommitCount(); got != 2 {
		t.Fatalf("commits = %d, want 2 (seq 1 closes the head gap and commits)", got)
	}
	if size, total := seq.receiveQueue.QueueSize(); size != 3 || total != 3*unit {
		t.Fatalf("hold = %d items / %d bytes, want 3 / %d", size, total, 3*unit)
	}
}

// The boundary commits exactly the prefix that can survive every gap below
// it filling, on the general (sorted) path: with a two-item hold under
// pressure the lower item commits and the upper one stays tentative, and
// both commit once the head drains.
func TestReceiveHoldCommittedPrefixCommitsOnlyBelowBoundary(t *testing.T) {
	const unit = ByteCount(1000)
	client, seq := newReceiveHoldTestSequence(t, ReceiveHoldCommittedPrefix, 3*unit+500)

	// D=0. seq2: missing 2, 2*1000 + 1000 = 3000 <= 3500 → commits.
	// seq3: missing 2, 2*1000 + 2000 = 4000 > 3500 → tentative.
	for _, n := range []uint64{2, 3} {
		if ok, err := seq.receive(holdPack(n, unit)); err != nil || !ok {
			t.Fatalf("receive seq %d: ok=%v err=%v", n, ok, err)
		}
	}

	if got := client.ReceiveQueueCommitCount(); got != 1 {
		t.Fatalf("commits = %d, want 1 (seq2 below the boundary, seq3 above it)", got)
	}
	if got := client.ReceiveQueueTentativeEvictionCount(); got != 0 {
		t.Fatalf("tentative evictions = %d, want 0 (hold not yet full)", got)
	}

	// The head drains 0 and 1: the delivery point moves to 2, so seq3's gap
	// shrinks to 1 and it commits on the next boundary walk.
	seq.nextSequenceNumber = 2
	seq.commitHeldPrefix()
	if got := client.ReceiveQueueCommitCount(); got != 2 {
		t.Fatalf("commits = %d, want 2 (seq3 crosses the boundary once the head drains)", got)
	}
}

// The whole-hold fast path (38d637c5): when the newest held item would
// survive every gap below it filling, the hold commits in one pass without
// the sort — observable as every held item committing on a single arrival.
func TestReceiveHoldCommittedPrefixFastPathCommitsWholeHold(t *testing.T) {
	const unit = ByteCount(1000)
	client, seq := newReceiveHoldTestSequence(t, ReceiveHoldCommittedPrefix, 6*unit)

	// D=0, hold seq1 only, then admit seq2. Newest = seq2: missing =
	// 2-0-1 = 1 → 1*1000 + 2000 = 3000 <= 6000 → the whole hold commits
	// through the unordered path.
	for _, n := range []uint64{1, 2} {
		if ok, err := seq.receive(holdPack(n, unit)); err != nil || !ok {
			t.Fatalf("receive seq %d: ok=%v err=%v", n, ok, err)
		}
	}

	if got := client.ReceiveQueueCommitCount(); got != 2 {
		t.Fatalf("commits = %d, want 2 (nothing is missing below seq1, so the hold is safe whole)", got)
	}
}

// The Evict arm is the pre-§37.20 behaviour, kept for comparison: it
// acknowledges on admission AND evicts, so its evictions are withdrawals.
func TestReceiveHoldEvictAcknowledgesOnAdmissionAndEvicts(t *testing.T) {
	const unit = ByteCount(1000)
	client, seq := newReceiveHoldTestSequence(t, ReceiveHoldEvict, 2*unit+500)

	// seq2 and seq3 fill the hold acked-on-admission; the missing seq 1
	// arrives and evicts the newest (seq3), which was already acknowledged.
	for _, n := range []uint64{2, 3} {
		if ok, err := seq.receive(holdPack(n, unit)); err != nil || !ok {
			t.Fatalf("receive seq %d: ok=%v err=%v", n, ok, err)
		}
	}
	if ok, err := seq.receive(holdPack(1, unit)); err != nil || !ok {
		t.Fatalf("receive seq 1: ok=%v err=%v", ok, err)
	}

	if got := client.ReceiveQueueEvictionCount(); got != 1 {
		t.Fatalf("committed evictions = %d, want 1 (the Evict arm withdraws acks)", got)
	}
	if got := client.ReceiveQueueCommitCount(); got != 0 {
		t.Fatalf("boundary commits = %d, want 0 (this arm does not walk a boundary)", got)
	}
	if size, _ := seq.receiveQueue.QueueSize(); size != 2 {
		t.Fatalf("hold size = %d, want 2 (seq 1 and seq 2)", size)
	}
}

// The Refuse arm never removes anything: a full hold refuses the arrival.
func TestReceiveHoldRefuseNeverEvicts(t *testing.T) {
	const unit = ByteCount(1000)
	client, seq := newReceiveHoldTestSequence(t, ReceiveHoldRefuse, 2*unit+500)

	for _, n := range []uint64{2, 3} {
		if ok, err := seq.receive(holdPack(n, unit)); err != nil || !ok {
			t.Fatalf("receive seq %d: ok=%v err=%v", n, ok, err)
		}
	}
	ok, err := seq.receive(holdPack(1, unit))
	if ok || err != nil {
		t.Fatalf("expected the refusal of seq1 (ok=false, nil error), got ok=%v err=%v", ok, err)
	}
	if size, _ := seq.receiveQueue.QueueSize(); size != 2 {
		t.Fatalf("hold size = %d, want 2 (refusal must not disturb the hold)", size)
	}
	if got := client.ReceiveQueueEvictionCount() + client.ReceiveQueueTentativeEvictionCount(); got != 0 {
		t.Fatalf("evictions = %d, want 0 under Refuse", got)
	}
}
