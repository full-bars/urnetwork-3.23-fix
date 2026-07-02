package main

import "sync"

// broadcaster is a minimal fan-out signal for the dashboard's live-update
// SSE stream: publish() wakes every subscribed channel with a bare
// notification (no payload) so a browser tab knows to re-fetch /api/nodes.
// Subscribers are size-1 buffered channels; publish never blocks on a slow
// or dead subscriber, and a channel with an already-pending notification
// just absorbs a burst of publishes into one pending wake.
type broadcaster struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan struct{}]bool)}
}

// subscribe registers a new size-1 buffered channel that receives a value
// on every publish() call until unsubscribe is called with it.
func (b *broadcaster) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

// unsubscribe removes ch from the subscriber set. Safe to call more than
// once or with a channel that was never subscribed.
func (b *broadcaster) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// publish wakes every current subscriber. Non-blocking: a subscriber whose
// buffered channel already holds a pending notification is left alone
// rather than blocking the publisher. Safe to call on a nil receiver
// (no-op) since most existing store tests construct &store{} directly
// without a broadcaster.
func (b *broadcaster) publish() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
