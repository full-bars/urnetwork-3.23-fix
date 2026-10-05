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

type h3GateState struct {
	mutex  sync.Mutex
	on     bool
	notify chan struct{}
}

func newH3Gate() *h3GateState {
	return &h3GateState{notify: make(chan struct{})}
}

// SetH3Enabled turns the auxiliary H3 transport on or off at runtime and
// returns the previous value. Turning it off closes any H3 connection that is
// up and stops further dials; turning it on lets the transports of eligible
// identities dial. A change wakes every waiting transport.
func SetH3Enabled(enabled bool) (previous bool) {
	h3Gate.mutex.Lock()
	defer h3Gate.mutex.Unlock()
	previous = h3Gate.on
	if previous != enabled {
		h3Gate.on = enabled
		close(h3Gate.notify)
		h3Gate.notify = make(chan struct{})
	}
	return previous
}

// H3Enabled reports whether the auxiliary H3 transport is switched on.
func H3Enabled() bool {
	h3Gate.mutex.Lock()
	defer h3Gate.mutex.Unlock()
	return h3Gate.on
}

// h3GateWatch returns the current state and a channel that closes when it next
// changes.
func h3GateWatch() (bool, <-chan struct{}) {
	h3Gate.mutex.Lock()
	defer h3Gate.mutex.Unlock()
	return h3Gate.on, h3Gate.notify
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
