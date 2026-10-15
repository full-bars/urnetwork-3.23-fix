package connect

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// The lifetime tests drive StreamSequence.Run through P2pTransportNewForTest,
// so no WebRTC setup is needed. The fake transports' contexts are children of
// the sequence's ctx, exactly like a real P2pTransport's, so external close,
// reset and replacement cancels cascade into the wait, and a test cancel of
// the child context alone models the transport giving up on its own. The reap
// hook is the only wait (a barrier), with a generous time.After that only
// turns a broken barrier into a failure. The seam runs on the sequence's own
// goroutine, so the recorder guards its slice with a mutex.
type transportRecorder struct {
	mu      sync.Mutex
	cancels []context.CancelFunc
}

func (rec *transportRecorder) add(cancel context.CancelFunc) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cancels = append(rec.cancels, cancel)
}

func (rec *transportRecorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.cancels)
}

// await spins (no sleeps) until n transports have been recorded; the seam runs
// on the sequence's goroutine, so creation is asynchronous to OpenStream.
func (rec *transportRecorder) await(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		if rec.count() >= n {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("transport %d was never created (have %d)", n, rec.count())
}

func (rec *transportRecorder) at(t *testing.T, i int) context.CancelFunc {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if i < 0 {
		i += len(rec.cancels)
	}
	if i < 0 || i >= len(rec.cancels) {
		t.Fatalf("no transport cancel recorded at %d (have %d)", i, len(rec.cancels))
	}
	return rec.cancels[i]
}

func newLifetimeTestBuffer(t *testing.T, settings *StreamBufferSettings) *StreamBuffer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewStreamBuffer(ctx, &StreamManager{}, settings)
}

func fakeChildTransport(sequence *StreamSequence) (*P2pTransport, context.CancelFunc) {
	ctx, cancel := context.WithCancel(sequence.ctx)
	return &P2pTransport{ctx: ctx}, cancel
}

func countStreamSequences(buffer *StreamBuffer) (int, int) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return len(buffer.streamSequences), len(buffer.streamSequencesByStreamId)
}

func awaitReap(t *testing.T, reaped <-chan *StreamSequence) *StreamSequence {
	t.Helper()
	select {
	case sequence := <-reaped:
		return sequence
	case <-time.After(10 * time.Second):
		t.Fatal("the reap signal must fire once the sequence exits")
		return nil
	}
}

// TestStreamSequenceEndsWhenItsTransportEnds: a single-ended sequence is
// reaped when its transport context ends while the parent stays live, and the
// transport-ended counter moves exactly once.
func TestStreamSequenceEndsWhenItsTransportEnds(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamId := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
		t.Fatalf("open must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 1)
	if a, b := countStreamSequences(buffer); a != 1 || b != 1 {
		t.Fatalf("the open stream must be registered once, got %d/%d", a, b)
	}
	reapedBefore := StreamSequencesReapedTransportEnded()

	// end the transport; the sequence must follow it
	rec.at(t, 0)()
	awaitReap(t, reaped)

	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the reaped sequence must leave the maps, got %d/%d", a, b)
	}
	if buffer.IsStreamOpen(streamId) {
		t.Fatal("the stream must read as closed after the reap")
	}
	if got := StreamSequencesLive(); got != 0 {
		t.Fatalf("no sequence may stay live, got %d", got)
	}
	if got := StreamSequencesReapedTransportEnded() - reapedBefore; got != 1 {
		t.Fatalf("the transport-ended counter must move exactly once, got %d", got)
	}
	select {
	case <-buffer.ctx.Done():
		t.Fatal("the parent context must stay live when only the transport ended")
	default:
	}
}

// The both-sides branch deliberately keeps parking on its own context in v33
// (review #3469); joining the two-ended case is v33.1 scope. Kept as a marker.
func TestStreamSequenceEndsWhenEitherTransportEnds(t *testing.T) {
	t.Skip("two-ended transport join is v33.1 scope (review #3469)")
}

// TestStreamSequenceKeepsItsTransportThroughUnrelatedOpenClose: opening and
// closing another stream must not touch this one.
func TestStreamSequenceKeepsItsTransportThroughUnrelatedOpenClose(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamIdA := NewId()
	streamIdB := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamIdA); err != nil || !opened {
		t.Fatalf("open A must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 1)
	if opened, err := buffer.OpenStream(nil, &destinationId, streamIdB); err != nil || !opened {
		t.Fatalf("open B must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 2)

	buffer.CloseStream(streamIdB)
	awaitReap(t, reaped)

	if a, b := countStreamSequences(buffer); a != 1 || b != 1 {
		t.Fatalf("only B may be reaped, got %d/%d", a, b)
	}
	if !buffer.IsStreamOpen(streamIdA) {
		t.Fatal("A must stay open through the unrelated close")
	}

	// cleanup: end A's transport (recorded first)
	rec.at(t, 0)()
	awaitReap(t, reaped)
	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("both sequences must be reaped, got %d/%d", a, b)
	}
}

// TestStreamBufferCloseStreamReapsTheSequence: the explicit close path still
// tears the sequence down (an external cancel, not a transport-ended reap).
func TestStreamBufferCloseStreamReapsTheSequence(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, _ := fakeChildTransport(sequence)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamId := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
		t.Fatalf("open must succeed, got opened=%v err=%v", opened, err)
	}
	reapedBefore := StreamSequencesReapedTransportEnded()

	buffer.CloseStream(streamId)
	awaitReap(t, reaped)

	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the closed sequence must leave the maps, got %d/%d", a, b)
	}
	if got := StreamSequencesReapedTransportEnded() - reapedBefore; got != 0 {
		t.Fatalf("an external close must not count as a transport-ended reap, got %d", got)
	}
}

// TestStreamBufferResetStreamsReapsSequences: reset cancels every sequence and
// every one is cleaned out of both maps.
func TestStreamBufferResetStreamsReapsSequences(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, _ := fakeChildTransport(sequence)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamIdA := NewId()
	streamIdB := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamIdA); err != nil || !opened {
		t.Fatalf("open A must succeed, got opened=%v err=%v", opened, err)
	}
	if opened, err := buffer.OpenStream(nil, &destinationId, streamIdB); err != nil || !opened {
		t.Fatalf("open B must succeed, got opened=%v err=%v", opened, err)
	}

	buffer.ResetStreams()
	awaitReap(t, reaped)
	awaitReap(t, reaped)

	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the reset must reap every sequence, got %d/%d", a, b)
	}
}

// TestStreamBufferReopensAStreamAfterItsSequenceIsReaped: ordering one - the
// reap completes first, then the platform reopens the same stream, which must
// build a fresh sequence that lives in both maps.
func TestStreamBufferReopensAStreamAfterItsSequenceIsReaped(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamId := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
		t.Fatalf("open must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 1)
	rec.at(t, 0)()
	first := awaitReap(t, reaped)
	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the reaped sequence must leave the maps, got %d/%d", a, b)
	}

	if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
		t.Fatalf("reopen must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 2)
	buffer.mutex.Lock()
	replacement := buffer.streamSequencesByStreamId[streamId]
	also := buffer.streamSequences[newStreamSequenceId(nil, &destinationId, streamId)]
	buffer.mutex.Unlock()
	if replacement == nil || also != replacement {
		t.Fatal("the reopened stream must be registered in both maps")
	}
	if replacement == first {
		t.Fatal("the reopened stream must build a fresh sequence")
	}

	rec.at(t, -1)()
	awaitReap(t, reaped)
	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the reaped replacement must leave the maps, got %d/%d", a, b)
	}
}

// TestStreamBufferReopenDuringReap: ordering two - the reopen races the reap.
// Whatever the interleaving, after both settle a live reopen lands a fresh
// sequence in both maps and the old cleanup never removes it.
func TestStreamBufferReopenDuringReap(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	destinationId := NewId()
	streamId := NewId()
	if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
		t.Fatalf("open must succeed, got opened=%v err=%v", opened, err)
	}

	rec.await(t, 1)

	// end the transport, then reopen immediately: the reopen may land before
	// or after the reap's map cleanup
	rec.at(t, 0)()
	buffer.OpenStream(nil, &destinationId, streamId)
	awaitReap(t, reaped)

	// settle to the invariant: the stream is open, and the fresh sequence is
	// in both maps
	if !buffer.IsStreamOpen(streamId) {
		if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
			t.Fatalf("reopen after the race must succeed, got opened=%v err=%v", opened, err)
		}
	}
	if a, b := countStreamSequences(buffer); a != 1 || b != 1 {
		t.Fatalf("exactly the fresh sequence must be registered, got %d/%d", a, b)
	}
	buffer.mutex.Lock()
	current := buffer.streamSequencesByStreamId[streamId]
	also := buffer.streamSequences[newStreamSequenceId(nil, &destinationId, streamId)]
	buffer.mutex.Unlock()
	if current == nil || also != current {
		t.Fatal("the fresh sequence must be in both maps after the race")
	}

	// cleanup: end the newest transport; the live sequence reaps like any
	// other. Exactly two transports exist by now: the original and the fresh
	// one (whichever of the racing reopen or the settle created it).
	rec.await(t, 2)
	rec.at(t, -1)()
	awaitReap(t, reaped)
	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the fresh sequence must reap the same way, got %d/%d", a, b)
	}
}

// TestStreamBufferReopenSameStreamIdDifferentSource: A's cleanup must not
// touch B when the same streamId is reopened with a different source.
func TestStreamBufferReopenSameStreamIdDifferentSource(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	sourceA := NewId()
	sourceB := NewId()
	streamId := NewId()
	if opened, err := buffer.OpenStream(&sourceA, nil, streamId); err != nil || !opened {
		t.Fatalf("open A must succeed, got opened=%v err=%v", opened, err)
	}
	rec.await(t, 1)
	reapedBefore := StreamSequencesReapedTransportEnded()

	// B carries the same streamId from a different source: opening it cancels
	// A, whose cleanup must then leave B's entries alone
	if opened, err := buffer.OpenStream(&sourceB, nil, streamId); err != nil || !opened {
		t.Fatalf("open B must succeed, got opened=%v err=%v", opened, err)
	}
	awaitReap(t, reaped)

	buffer.mutex.Lock()
	b := buffer.streamSequencesByStreamId[streamId]
	aEntry := buffer.streamSequences[newStreamSequenceId(&sourceA, nil, streamId)]
	bEntry := buffer.streamSequences[newStreamSequenceId(&sourceB, nil, streamId)]
	buffer.mutex.Unlock()
	if b == nil || bEntry != b || bEntry == nil {
		t.Fatal("B must be in both maps after A's cleanup")
	}
	if aEntry != nil {
		t.Fatal("A's entry must be gone")
	}
	if !buffer.IsStreamOpen(streamId) {
		t.Fatal("the stream must stay open through A's cleanup")
	}

	// A was reaped by an external replacement, not by a transport ending
	if got := StreamSequencesReapedTransportEnded() - reapedBefore; got != 0 {
		t.Fatalf("the replacement reap must not count as transport-ended, got %d", got)
	}

	rec.await(t, 2)
	rec.at(t, -1)()
	awaitReap(t, reaped)
	if a, b := countStreamSequences(buffer); a != 0 || b != 0 {
		t.Fatalf("the reaped replacement must leave the maps, got %d/%d", a, b)
	}
}

// TestStreamSequenceReapReturnsGoroutinesToBaseline: repeated open-and-reap
// cycles must not accumulate goroutines.
func TestStreamSequenceReapReturnsGoroutinesToBaseline(t *testing.T) {
	reaped := make(chan *StreamSequence, 8)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) {
			reaped <- sequence
		},
	}
	buffer := newLifetimeTestBuffer(t, settings)

	baseline := runtime.NumGoroutine()
	destinationId := NewId()
	for i := 0; i < 5; i++ {
		streamId := NewId()
		if opened, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil || !opened {
			t.Fatalf("open must succeed, got opened=%v err=%v", opened, err)
		}
		rec.await(t, i+1)
		rec.at(t, -1)()
		awaitReap(t, reaped)
	}

	// the wrapper goroutines exit right after the reap hook; yield until they
	// have, with a bounded budget instead of a sleep
	for i := 0; i < 100000; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("goroutines must return to baseline %d, got %d", baseline, runtime.NumGoroutine())
}
