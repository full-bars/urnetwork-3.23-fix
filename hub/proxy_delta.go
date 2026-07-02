package main

import (
	"fmt"
	"sync"
)

// proxyCounters holds the cumulative counters from one proxyReport.
type proxyCounters struct {
	RX, TX, BillRX, BillTX uint64
	Acq, Denied            int64
	Clients                int64
}

// deltaTracker converts cumulative per-(node,proxy) counters into
// per-interval deltas. Providers report totals since proxy start, so the
// hub must diff consecutive reports; a counter going backwards means the
// provider restarted and the new value becomes the baseline.
type deltaTracker struct {
	mu   sync.Mutex
	prev map[string]proxyCounters // nodeID + "|" + addr
}

func newDeltaTracker() *deltaTracker {
	return &deltaTracker{prev: map[string]proxyCounters{}}
}

// delta returns the increase since the previous report for this
// (node, proxy) pair. On first sighting, or when any counter went
// backwards (provider restart), it records cur as the new baseline and
// returns ok=false — callers must not write a row for that report.
func (d *deltaTracker) delta(nodeID, addr string, cur proxyCounters) (proxyCounters, bool) {
	key := nodeID + "|" + addr
	d.mu.Lock()
	defer d.mu.Unlock()

	prev, seen := d.prev[key]
	d.prev[key] = cur
	if !seen ||
		cur.RX < prev.RX || cur.TX < prev.TX ||
		cur.BillRX < prev.BillRX || cur.BillTX < prev.BillTX ||
		cur.Acq < prev.Acq || cur.Denied < prev.Denied {
		return proxyCounters{}, false
	}
	return proxyCounters{
		RX:     cur.RX - prev.RX,
		TX:     cur.TX - prev.TX,
		BillRX: cur.BillRX - prev.BillRX,
		BillTX: cur.BillTX - prev.BillTX,
		Acq:    cur.Acq - prev.Acq,
		Denied: cur.Denied - prev.Denied,
		// Clients is a gauge, not a counter: pass through for peak tracking.
		Clients: cur.Clients,
	}, true
}

// forgetNode drops all baselines for a node (used when a node is removed,
// so a re-added node starts fresh instead of producing a giant delta).
func (d *deltaTracker) forgetNode(nodeID string) {
	prefix := nodeID + "|"
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.prev {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(d.prev, k)
		}
	}
}

// internProxy returns the stable integer id for addr, creating it on first
// use. Ids are cached in memory; the DB UNIQUE constraint keeps them stable
// across restarts. Caller must hold s.mu (upsert does).
func (s *store) internProxy(addr string) (int64, error) {
	if id, ok := s.proxyIDs[addr]; ok {
		return id, nil
	}
	if addr == "" {
		return 0, fmt.Errorf("empty proxy address")
	}

	var id int64
	err := s.db.QueryRow(`
		INSERT INTO proxies(addr) VALUES(?)
		ON CONFLICT(addr) DO UPDATE SET addr=excluded.addr
		RETURNING id`, addr).Scan(&id)
	if err != nil {
		return 0, err
	}

	s.proxyIDs[addr] = id
	return id, nil
}
