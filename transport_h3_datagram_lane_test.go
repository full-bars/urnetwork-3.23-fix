package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// The lane must never make its caller wait: when a send is held up, the lane
// fills and the next hand-off reports congestion instead of stalling the
// dispatcher that also feeds the reliable stream. The payload is copied, because
// the fragmenter sends every fragment of one message through a reused scratch
// buffer.
func TestH3DatagramSendLaneIsBoundedAndCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	sent := make(chan []byte, 64)
	lane := newH3DatagramSender(ctx, func(payload []byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		sent <- payload
		return nil
	}, nil)

	payload := []byte{0x01, 0x02}
	if !lane.trySend(payload) {
		t.Fatal("an empty lane must take the first datagram")
	}
	// The lane's goroutine now holds that datagram in its blocking send, so the
	// queue is empty with exactly its capacity free.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the lane never started sending")
	}
	for i := 0; i < h3DatagramSendLaneDepth; i++ {
		if !lane.trySend(payload) {
			t.Fatalf("the lane refused datagram %d of its %d slots", i+1, h3DatagramSendLaneDepth)
		}
	}

	done := make(chan bool, 1)
	go func() { done <- lane.trySend(payload) }()
	select {
	case took := <-done:
		if took {
			t.Fatal("a full lane must refuse rather than queue more")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trySend waited on a full lane: the dispatcher would have stalled")
	}

	// The hand-off copied, so mutating the caller's buffer afterwards must not
	// change what the lane sends.
	payload[0] = 0x7f
	close(release)
	select {
	case got := <-sent:
		if got[0] != 0x01 {
			t.Fatalf("the lane sent the caller's later buffer contents: got %#x, want 0x01", got[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was sent")
	}
}

// The lane belongs to one connection generation: cancelling its context ends the
// goroutine, and whatever is still queued is dropped, which is what an unreliable
// lane is for.
func TestH3DatagramSendLaneStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan struct{})
	release := make(chan struct{})
	lane := newH3DatagramSender(ctx, func([]byte) error {
		close(blocked)
		<-release
		return nil
	}, nil)
	if !lane.trySend([]byte{0x01}) {
		t.Fatal("an empty lane must take the first datagram")
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the lane never started sending")
	}
	close(release)

	cancel()
	select {
	case <-lane.closed():
	case <-time.After(2 * time.Second):
		t.Fatal("the lane outlived its context")
	}
	// A stopped lane refuses rather than panicking or blocking.
	if lane.trySend([]byte{0x02}) {
		t.Fatal("a cancelled lane accepted a datagram: it has no reader left")
	}
}

// The lane sends asynchronously, so an error quic-go returned synchronously now
// arrives on the report callback. That is how a too-large path report reaches the
// live limit and how the send-error metric keeps counting; swallowing it left both
// blind.
func TestH3DatagramSendLaneReportsSendErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reported := make(chan error, 1)
	want := &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 512}
	lane := newH3DatagramSender(ctx, func([]byte) error { return want }, func(err error) {
		reported <- err
	})
	if !lane.trySend([]byte{0x01}) {
		t.Fatal("an empty lane must take the first datagram")
	}
	select {
	case err := <-reported:
		var tooLarge *quic.DatagramTooLargeError
		if !errors.As(err, &tooLarge) || tooLarge.MaxDatagramPayloadSize != 512 {
			t.Fatalf("reported %v, want the too-large error intact", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a send error was swallowed: nothing can lower the path limit")
	}
}

// Congestion is the case this lane exists for, so a full lane must refuse without
// paying for the copy first: otherwise every frame the path cannot carry allocates
// ~1.3 KB and immediately drops it for the collector.
func TestH3DatagramSendLaneNoAllocWhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	lane := newH3DatagramSender(ctx, func([]byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}, nil)

	payload := []byte{0x01, 0x02}
	if !lane.trySend(payload) {
		t.Fatal("an empty lane must take the first datagram")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the lane never started sending")
	}
	for i := 0; i < h3DatagramSendLaneDepth; i++ {
		if !lane.trySend(payload) {
			t.Fatalf("the lane refused datagram %d of its %d slots", i+1, h3DatagramSendLaneDepth)
		}
	}

	refused := 0
	allocs := testing.AllocsPerRun(200, func() {
		if lane.trySend(payload) {
			refused++
		}
	})
	if refused != 0 {
		t.Fatal("a full lane took a datagram")
	}
	if allocs != 0 {
		t.Fatalf("a full lane must refuse without allocating, got %.1f allocs per call", allocs)
	}
	close(release)
}

// A path report from the lane may only ever lower the live limit: a larger or
// nonsensical report must not raise it, or the dispatcher would keep offering
// datagrams the path cannot carry.
func TestLowerDatagramPathLimitOnlyLowers(t *testing.T) {
	var live atomic.Int64
	live.Store(1360)

	if !lowerDatagramPathLimit(&live, 1200) || live.Load() != 1200 {
		t.Fatalf("a smaller reported limit must be recorded, live=%d", live.Load())
	}
	if lowerDatagramPathLimit(&live, 1400) || live.Load() != 1200 {
		t.Fatalf("a larger report must not raise the limit, live=%d", live.Load())
	}
	if lowerDatagramPathLimit(&live, 1200) {
		t.Fatal("an equal report is not a change")
	}
	if lowerDatagramPathLimit(&live, 0) || lowerDatagramPathLimit(&live, -5) || live.Load() != 1200 {
		t.Fatalf("a nonsensical report must be ignored, live=%d", live.Load())
	}
}
