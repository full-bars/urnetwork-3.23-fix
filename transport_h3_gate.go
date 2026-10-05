package connect

import (
	"sync"
)

// h3Gate is the process-wide runtime switch for the auxiliary H3 transport.
// PlatformTransportSettings.EnableH3 says an identity is ELIGIBLE (a direct
// identity whose host UDP socket is the right one); the gate says whether H3
// actually runs right now. It exists so an operator can turn H3 on and off
// without restarting the provider, which is what makes a same-box A/B
// comparison affordable: a restart drops every proxy and its clients, so
// interleaved on and off windows would otherwise measure the restarts.
//
// It defaults to off, so an eligible identity opens no UDP socket and starts no
// QUIC connection until something turns it on.
var h3Gate = newH3Gate()

// h3DatagramGate is the runtime switch for OFFERING QUIC DATAGRAM on the
// auxiliary H3 connection. It defaults to off, and it only has an effect while
// H3 itself is on. The offer is made when a connection is dialed, so a change
// closes the live H3 connection and lets it reconnect with the new setting.
var h3DatagramGate = newH3Gate()

type h3GateState struct {
	mutex  sync.Mutex
	on     bool
	notify chan struct{}
}

func newH3Gate() *h3GateState {
	return &h3GateState{notify: make(chan struct{})}
}

func (self *h3GateState) set(enabled bool) (previous bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	previous = self.on
	if previous != enabled {
		self.on = enabled
		close(self.notify)
		self.notify = make(chan struct{})
	}
	return previous
}

func (self *h3GateState) get() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.on
}

// watch returns the current state and a channel that closes when it next
// changes.
func (self *h3GateState) watch() (bool, <-chan struct{}) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.on, self.notify
}

// SetH3Enabled turns the auxiliary H3 transport on or off at runtime and
// returns the previous value. Turning it off closes any H3 connection that is
// up and stops further dials; turning it on lets the transports of eligible
// identities dial. A change wakes every waiting transport.
func SetH3Enabled(enabled bool) (previous bool) {
	return h3Gate.set(enabled)
}

// H3Enabled reports whether the auxiliary H3 transport is switched on.
func H3Enabled() bool {
	return h3Gate.get()
}

// h3GateWatch returns the current state and a channel that closes when it next
// changes.
func h3GateWatch() (bool, <-chan struct{}) {
	return h3Gate.watch()
}

// SetH3DatagramsEnabled switches the offer of QUIC DATAGRAM on the H3
// connection and returns the previous value. See h3DatagramGate.
func SetH3DatagramsEnabled(enabled bool) (previous bool) {
	return h3DatagramGate.set(enabled)
}

// H3DatagramsEnabled reports whether H3 offers QUIC DATAGRAM.
func H3DatagramsEnabled() bool {
	return h3DatagramGate.get()
}

// waitH3Enabled blocks until the gate is on. It returns false if the transport
// is closed first. A transport that is not gated (the explicit H3 target mode
// is the sole transport) never waits.
func (self *PlatformTransport) waitH3Enabled() bool {
	if !self.h3Gated() {
		return true
	}
	for {
		on, notify := h3GateWatch()
		if on {
			return true
		}
		select {
		case <-self.ctx.Done():
			return false
		case <-notify:
		}
	}
}
