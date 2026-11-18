package gossip

import (
	"strings"
	"sync"
	"time"
)

const EventCapacity = 1000

// Event contains metadata only. It never contains a message payload or address.
type Event struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Peer      string    `json:"peer,omitempty"`
	Direction string    `json:"direction,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Size      int       `json:"size,omitempty"`
}

// EventRing keeps a bounded history. Writers drop an event rather than wait
// for a concurrent snapshot or writer.
type EventRing struct {
	mu     sync.Mutex
	events [EventCapacity]Event
	next   int
	count  int
}

func eventField(s string, limit int) string {
	if len(s) > limit {
		s = s[:limit]
	}
	return strings.Clone(s)
}

// Record returns false when another operation holds the ring.
func (r *EventRing) Record(e Event) bool {
	if !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	e.Kind = eventField(e.Kind, 16)
	e.Peer = eventField(e.Peer, 8)
	e.Direction = eventField(e.Direction, 16)
	e.Reason = eventField(e.Reason, 64)
	r.events[r.next] = e
	r.next = (r.next + 1) % len(r.events)
	if r.count < len(r.events) {
		r.count++
	}
	return true
}

// Snapshot returns events in insertion order without exposing the ring.
func (r *EventRing) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, r.count)
	start := (r.next - r.count + len(r.events)) % len(r.events)
	for i := range out {
		out[i] = r.events[(start+i)%len(r.events)]
	}
	return out
}
