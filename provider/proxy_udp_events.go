package main

import (
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// ProxyUDPScanEvent records one finished UDP check for urnet-tools top's scan wave animation.
type ProxyUDPScanEvent struct {
	Index  int       `json:"index"`
	Status string    `json:"status"` // "ok", "no_relay", "mismatch", "unknown"
	At     time.Time `json:"at"`
}

const defaultProxyUDPScanRingCap = 256

// proxyUDPScanRing is a bounded ring buffer of recent finished UDP check events.
type proxyUDPScanRing struct {
	mu     sync.Mutex
	events []ProxyUDPScanEvent
	head   int
	count  int
}

func newProxyUDPScanRing(capacity int) *proxyUDPScanRing {
	if capacity <= 0 {
		capacity = defaultProxyUDPScanRingCap
	}
	return &proxyUDPScanRing{
		events: make([]ProxyUDPScanEvent, capacity),
	}
}

func (r *proxyUDPScanRing) Record(index int, status string, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := (r.head + r.count) % len(r.events)
	if r.count == len(r.events) {
		r.head = (r.head + 1) % len(r.events)
	} else {
		r.count++
	}
	r.events[idx] = ProxyUDPScanEvent{
		Index:  index,
		Status: status,
		At:     at,
	}
}

func (r *proxyUDPScanRing) Snapshot() []ProxyUDPScanEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	out := make([]ProxyUDPScanEvent, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.events[(r.head+i)%len(r.events)]
	}
	return out
}

func (self *proxyUDPCheckStore) RecentEvents() []ProxyUDPScanEvent {
	if self == nil || self.events == nil {
		return nil
	}
	return self.events.Snapshot()
}

func (self *proxyUDPCheckStore) recordEvent(settings *connect.ProxySettings, result proxyUDPResult, now time.Time) {
	if self == nil || self.events == nil || settings == nil {
		return
	}
	idx := settings.Index
	if idx <= 0 {
		proxyStateMu.Lock()
		if state, err := readProxyState(); err == nil && state != nil {
			if entry, ok := state.Proxies[settings.Key()]; ok {
				idx = entry.ID
			}
		}
		proxyStateMu.Unlock()
	}
	var statusStr string
	switch result.Status {
	case proxyUDPOK:
		statusStr = "ok"
	case proxyUDPNoRelay:
		statusStr = "no_relay"
	case proxyUDPExitMismatch:
		statusStr = "mismatch"
	default:
		statusStr = "unknown"
	}
	self.events.Record(idx, statusStr, now)
}
