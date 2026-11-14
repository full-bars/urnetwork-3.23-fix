package extender

import (
	"context"
	"net"
	"sync"

	"github.com/urnetwork/connect"
)

// Admission and completion share a lock with handler replacement, so a
// detached handler cannot be admitted after its setter returns.
type extenderServiceHandlers struct {
	mu      sync.Mutex
	gossip  func(net.Conn)
	feed    func(net.Conn)
	active  int
	drained chan struct{}
}

// SetGossipConnHandler replaces the gossip handler. Nil refuses new streams.
// Streams admitted before the change keep their original handler.
func (self *ExtenderServer) SetGossipConnHandler(handler func(net.Conn)) {
	self.serviceHandlers.mu.Lock()
	defer self.serviceHandlers.mu.Unlock()
	self.serviceHandlers.gossip = handler
}

// SetFeedConnHandler replaces the feed handler. Nil refuses new streams.
// Streams admitted before the change keep their original handler.
func (self *ExtenderServer) SetFeedConnHandler(handler func(net.Conn)) {
	self.serviceHandlers.mu.Lock()
	defer self.serviceHandlers.mu.Unlock()
	self.serviceHandlers.feed = handler
}

// WaitServiceStreams waits until all admitted gossip and feed requests have
// finished, including requests still taking over their streams. It returns the
// context error if the wait is cancelled; it does not close streams. Detach
// both handlers before waiting to prevent new admissions from extending the
// wait. Do not call it from a service handler that is being waited for.
func (self *ExtenderServer) WaitServiceStreams(ctx context.Context) error {
	h := &self.serviceHandlers
	h.mu.Lock()
	if h.active == 0 {
		h.mu.Unlock()
		return nil
	}
	drained := h.drained
	h.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *ExtenderServer) admitServiceStream(service uint32) func(net.Conn) {
	h := &self.serviceHandlers
	h.mu.Lock()
	defer h.mu.Unlock()
	var handler func(net.Conn)
	switch service {
	case connect.ExtenderServiceGossip:
		handler = h.gossip
	case connect.ExtenderServiceFeed:
		handler = h.feed
	}
	if handler != nil {
		if h.active == 0 {
			h.drained = make(chan struct{})
		}
		h.active++
	}
	return handler
}

func (self *ExtenderServer) endServiceStream() {
	h := &self.serviceHandlers
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	if h.active == 0 {
		close(h.drained)
	}
}
