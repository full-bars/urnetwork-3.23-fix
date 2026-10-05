package connect

import (
	"context"
	"sync/atomic"
)

// h3DatagramSendLaneDepth is how many datagrams may be waiting on the lane's
// goroutine. Datagrams are unreliable: a full lane is congestion, and the caller
// takes the reliable stream instead of waiting.
const h3DatagramSendLaneDepth = 16

// h3DatagramSender carries datagrams on a goroutine of its own.
//
// quic-go's Connection.SendDatagram does not fail when its datagram flight is
// full, it blocks until a slot frees, and it takes no context. Calling it from
// the dispatcher that also feeds the reliable stream therefore lets a peer that
// stops draining datagrams hold up stream frames behind it: an unreliable lane
// stalling a reliable one. Here the blocking call happens on this goroutine, and
// the dispatcher only ever hands over a datagram it can hand over immediately.
type h3DatagramSender struct {
	send    chan []byte
	done    chan struct{}
	stopped atomic.Bool
}

// newH3DatagramSender starts the lane. The goroutine exits when ctx ends, and
// anything still queued is dropped, which is what an unreliable lane is for.
func newH3DatagramSender(ctx context.Context, send func([]byte) error) *h3DatagramSender {
	self := &h3DatagramSender{
		send: make(chan []byte, h3DatagramSendLaneDepth),
		done: make(chan struct{}),
	}
	go func() {
		defer func() {
			self.stopped.Store(true)
			close(self.done)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-self.send:
				// A failure here is the lane's own; the guard and the connection's
				// failure handling own the outcome, so the datagram is dropped.
				_ = send(payload)
			}
		}
	}()
	return self
}

// trySend hands one datagram to the lane without waiting, and reports whether it
// was taken. The payload is copied: the fragmenter sends every fragment of a
// message through one reused scratch buffer, so an owned copy is what makes an
// asynchronous send safe.
func (self *h3DatagramSender) trySend(payload []byte) bool {
	// A stopped lane has no reader: taking the datagram would only park it in a
	// channel nobody drains.
	if self.stopped.Load() {
		return false
	}
	owned := make([]byte, len(payload))
	copy(owned, payload)
	select {
	case self.send <- owned:
		return true
	default:
		return false
	}
}

// closed reports whether the lane's goroutine has ended, so a test can assert the
// lane goes away with its connection generation.
func (self *h3DatagramSender) closed() <-chan struct{} {
	return self.done
}
