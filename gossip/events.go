package gossip

import (
	"strings"
	"sync"
	"time"
)

const (
	EventCapacity            = 1000
	DefaultRejectSampleEvery = 16
)

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
	mu       sync.Mutex
	events   [EventCapacity]Event
	next     int
	count    int
	capTimes [3]time.Time
}

func eventField(s string, limit int) string {
	if len(s) > limit {
		s = s[:limit]
	}
	return strings.Clone(s)
}

// Record returns false when another operation holds the ring.
func (r *EventRing) Record(e Event) bool {
	if r == nil {
		return false
	}
	if !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	r.recordLocked(e)
	return true
}

// RecordCap limits repeated cap events by reason while retaining the first hit.
func (r *EventRing) RecordCap(e Event) bool {
	if r == nil || !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	var i int
	switch e.Reason {
	case "subscriber_cap":
		i = 0
	case "accept_queue_full":
		i = 1
	case "resource_limit":
		i = 2
	default:
		r.recordLocked(e)
		return true
	}
	last := r.capTimes[i]
	if !last.IsZero() && !e.Time.Before(last) && e.Time.Sub(last) < time.Minute {
		return false
	}
	r.capTimes[i] = e.Time
	r.recordLocked(e)
	return true
}

func (r *EventRing) recordLocked(e Event) {
	e.Kind = eventField(e.Kind, 16)
	if len(e.Peer) > 8 {
		e.Peer = e.Peer[len(e.Peer)-8:]
	}
	e.Peer = eventField(e.Peer, 8)
	e.Direction = eventField(e.Direction, 16)
	e.Reason = eventField(e.Reason, 64)
	r.events[r.next] = e
	r.next = (r.next + 1) % len(r.events)
	if r.count < len(r.events) {
		r.count++
	}
}

// Snapshot returns events in insertion order without exposing the ring.
func (r *EventRing) Snapshot() []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, r.count)
	start := (r.next - r.count + len(r.events)) % len(r.events)
	for i := range out {
		out[i] = r.events[(start+i)%len(r.events)]
	}
	return out
}
