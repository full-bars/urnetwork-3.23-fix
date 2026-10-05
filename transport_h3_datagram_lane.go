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
//
// Sending here is asynchronous, so the errors quic-go returns synchronously are
// asynchronous too. onSendError reports each one so the caller can lower the path
// limit on a too-large report and keep counting failures.
type h3DatagramSender struct {
	ctx    context.Context
	send   chan []byte
	done   chan struct{}
	report func(error)

	stopped atomic.Bool
}

// newH3DatagramSender starts the lane. The goroutine exits when ctx ends, and
// anything still queued is dropped, which is what an unreliable lane is for.
func newH3DatagramSender(ctx context.Context, send func([]byte) error, report func(error)) *h3DatagramSender {
	self := &h3DatagramSender{
		ctx:    ctx,
		send:   make(chan []byte, h3DatagramSendLaneDepth),
		done:   make(chan struct{}),
		report: report,
	}
	go HandleError(func() {
		defer func() {
			self.stopped.Store(true)
			close(self.done)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-self.send:
				if err := send(payload); err != nil && self.report != nil {
					self.report(err)
				}
			}
		}
	})
	return self
}

// lowerDatagramPathLimit records a path limit quic-go reported from the lane
// goroutine. It only ever lowers the live limit and ignores a nonsensical report,
// so the dispatcher's next frame takes the stream rather than a datagram the path
// can no longer carry.
func lowerDatagramPathLimit(live *atomic.Int64, reported int64) bool {
	if reported <= 0 || live.Load() <= reported {
		return false
	}
	live.Store(reported)
	return true
}

// trySend hands one datagram to the lane without waiting, and reports whether it
// was taken. The payload is copied: the fragmenter sends every fragment of a
// message through one reused scratch buffer, so an owned copy is what makes an
// asynchronous send safe.
func (self *h3DatagramSender) trySend(payload []byte) bool {
	// A stopped or cancelled lane has no reader: taking the datagram would only
	// park it in a channel nobody drains, while the caller counted it as sent.
	if self.stopped.Load() || self.ctx.Err() != nil {
		return false
	}
	// Congestion is the common case here, so refuse before paying for the copy.
	if cap(self.send) <= len(self.send) {
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
