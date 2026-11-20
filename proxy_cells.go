package connect

import "time"

// Proxy grid cell state codes. They are the wire values the provider sends for
// each proxy and the client decodes; keep them in step with proxyCellStateName.
const (
	ProxyCellUp = iota
	ProxyCellDegraded
	ProxyCellConnecting
	ProxyCellDead
	ProxyCellParked
)

// ProxyCellRegistration identifies a registry slot incarnation and identity.
// Its opaque fields allow callers to compare samples without exposing keys.
type ProxyCellRegistration struct {
	health   *proxyHealth
	revision uint64
}

// ProxyCellRaw is one proxy's compact runtime state, read under a single
// registry lock for the proxy grid. Index is the registry index (stable for
// the life of the registration); Addr is the dial target for display only;
// Billable and Total are cumulative byte counters the caller differences to
// bucket throughput; Clients is the live session count; Age is how long the
// proxy has been continuously present (its serving window), zero when unknown.
type ProxyCellRaw struct {
	Registration ProxyCellRegistration
	Index        int
	State        uint8
	Addr         string
	// Key is the proxy's identity, the key the provider's UDP check store uses.
	Key                                      string
	Billable                                 uint64
	Total                                    uint64
	TotalRx, TotalTx, BillableRx, BillableTx uint64
	HasBandwidth                             bool
	Clients                                  int64
	Age                                      time.Duration
}

// ProxyCells reads one compact record per registered proxy, index-sorted,
// under a single registry lock. It is the cheap input to the proxy grid: one
// lock, one output slice, and no per-proxy allocation beyond that slice and its
// address strings. It never mutates the registry (no baseline advance), so it
// is safe to call as often as the grid needs.
func ProxyCells(now time.Time) []ProxyCellRaw {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	out := make([]ProxyCellRaw, 0, len(proxyHealthByIndex))
	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]
		c := ProxyCellRaw{Index: idx, Addr: h.address, Key: h.key, Registration: ProxyCellRegistration{health: h, revision: h.cellRevision}}
		switch {
		case h.parked:
			// Parked is its own state, checked before connectingActive: a
			// parked proxy still reads connecting to that predicate, and the
			// grid must tell "resting under the attempt budget" from "trying
			// to connect".
			c.State = ProxyCellParked
		case h.currentlyUp:
			c.State = ProxyCellUp
		case h.connectingActive(now):
			c.State = ProxyCellConnecting
		case h.everUp:
			if !h.downSince.IsZero() && now.Sub(h.downSince) >= 7*24*time.Hour {
				c.State = ProxyCellDead
			} else {
				c.State = ProxyCellDegraded
			}
		default:
			c.State = ProxyCellDead
		}
		if h.bw != nil {
			c.HasBandwidth = true
			c.BillableRx, c.BillableTx = h.bw.BillableRx.Load(), h.bw.BillableTx.Load()
			c.TotalRx, c.TotalTx = h.bw.TotalRx.Load(), h.bw.TotalTx.Load()
			c.Billable = c.BillableRx + c.BillableTx
			c.Total = c.TotalRx + c.TotalTx
			c.Clients = h.bw.Clients.Load()
			c.Age = h.bw.MaxAge()
		}
		out = append(out, c)
	}
	return out
}
