package connect

import (
	"sync/atomic"
	"time"
)

// The send guard protects against a datagram blackhole. Datagrams are
// unreliable by design and carry no acknowledgement of their own, so a path that
// silently eats them looks exactly like a working one from this side: every
// small frame, including acks and contract control, is lost, nothing reports it,
// and Transfer gives up on the sequence only after its ack timeout.
//
// A working datagram carrier is not silent. The server answers small frames
// (acks above all) as datagrams too, so once this side has been sending
// datagrams a healthy connection receives some. The guard watches for the
// opposite: many datagrams sent, none received, while the stream is plainly
// alive. When that holds the datagram lane is switched off for the rest of the
// connection and everything goes on the stream, which is the behaviour before
// datagram send existed, so the failure mode is "back to normal", not "stuck".
var (
	// how many datagrams may go unanswered before the guard can trip
	h3DatagramGuardMinSent int64 = 50
	// and for how long they must have gone unanswered
	h3DatagramGuardWindow = 20 * time.Second
)

type h3DatagramSendGuard struct {
	minSent int64
	window  time.Duration

	sinceReceive    atomic.Int64 // datagrams sent since the last one received
	firstUnanswered atomic.Int64 // unix nanos of the first unanswered send
	tripped         atomic.Bool
}

func newH3DatagramSendGuard() *h3DatagramSendGuard {
	return &h3DatagramSendGuard{minSent: h3DatagramGuardMinSent, window: h3DatagramGuardWindow}
}

// noteSent records one datagram sent.
func (self *h3DatagramSendGuard) noteSent(now time.Time) {
	n := self.sinceReceive.Add(1)
	if n == 1 {
		self.firstUnanswered.Store(now.UnixNano())
		return
	}
	// Below the threshold, an idle gap longer than the window means the earlier
	// sends are not a blackhole in progress: they are a datagram sent long ago
	// plus a fresh burst. Re-peg the clock so the burst is measured from its own
	// start instead of from that lone send, which would trip the guard on the
	// burst's first frames.
	if n < self.minSent && self.window < now.Sub(time.Unix(0, self.firstUnanswered.Load())) {
		self.firstUnanswered.Store(now.UnixNano())
	}
}

// noteReceived records one datagram received: the lane is alive.
func (self *h3DatagramSendGuard) noteReceived() {
	self.sinceReceive.Store(0)
}

// allow reports whether the next message may go as a datagram. streamAlive says
// the stream received something within the window: without that the connection
// is dead rather than the datagram lane, and the connection's own failure
// handling owns it. It returns false for good once the guard has tripped, and
// trippedNow is true only on the call that tripped it.
func (self *h3DatagramSendGuard) allow(now time.Time, streamAlive bool) (ok bool, trippedNow bool) {
	if self.tripped.Load() {
		return false, false
	}
	if self.minSent <= self.sinceReceive.Load() && streamAlive &&
		self.window <= now.Sub(time.Unix(0, self.firstUnanswered.Load())) {
		if self.tripped.CompareAndSwap(false, true) {
			return false, true
		}
		return false, false
	}
	return true, false
}

// Tripped reports whether the guard has switched the datagram lane off.
func (self *h3DatagramSendGuard) Tripped() bool {
	return self.tripped.Load()
}
