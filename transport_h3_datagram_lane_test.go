package connect

import (
	"context"
	"testing"
	"time"
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
	})

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
	})
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
