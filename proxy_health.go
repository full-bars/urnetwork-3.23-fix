package connect

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyBandwidth tracks the data usage of a proxy.
type ProxyBandwidth struct {
	TotalRx, TotalTx, BillableRx, BillableTx atomic.Uint64
	Clients atomic.Int64
}

// proxyHealth tracks one proxy's platform-transport liveness for the
// [health][proxies] report. See docs/design/dead-proxy-health-report.md.
type proxyHealth struct {
	address     string
	currentlyUp bool
	everUp      bool
	downSince   time.Time // when currentlyUp last went false (for recovery latency)
	lastSeenUp  bool      // currentlyUp as of the previous heartbeat (baseline)
	deadLogged  bool      // a confirmed-dead event has been emitted for this proxy
	bw          *ProxyBandwidth
}


// ProxyEvent identifies a proxy in a transition list. After is set for
// recovered events (time the proxy was down before coming back).
type ProxyEvent struct {
	Index   int
	Address string
	After   time.Duration
}

// ProxyHealthReport is the full per-heartbeat result.
type ProxyHealthReport struct {
	Up       int
	Dead     []string // formatted "proxy[idx] (addr)", index-sorted, complete (uncapped)
	Degraded []string

	Recovered     []ProxyEvent // down->up since last heartbeat
	NewlyDegraded []ProxyEvent // up->down since last heartbeat
	NewlyDead     []ProxyEvent // never-up proxies newly confirmed dead (logged once)

	LifetimeRecovered int
	LifetimeLost      int

	Bandwidth map[string]ProxyBandwidth
}

var (
	proxyHealthMu      sync.Mutex
	proxyHealthByIndex = map[int]*proxyHealth{}

	proxyLifetimeRecovered int
	proxyLifetimeLost      int
	proxyBaselineSet       bool
)

// RegisterProxy adds a proxy to the registry if absent. Idempotent so a list
// re-read preserves everUp. Called eagerly at startup for every proxy.
func RegisterProxy(index int, address string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if _, ok := proxyHealthByIndex[index]; !ok {
		proxyHealthByIndex[index] = &proxyHealth{address: address}
	}
}

// RegisterProxyBandwidth securely retrieves or initializes the proxyBandwidth.
func RegisterProxyBandwidth(index int) *ProxyBandwidth {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		// Initialize it if it doesn't exist
		h = &proxyHealth{address: ""}
		proxyHealthByIndex[index] = h
	}
	if h.bw == nil {
		h.bw = &ProxyBandwidth{}
	}
	return h.bw
}

// markProxyUp records that the proxy's platform transport is live.
func markProxyUp(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.currentlyUp = true
		h.everUp = true
	}
}

// markProxyDown records that the proxy's platform transport went down, stamping
// downSince when it was previously up (for recovery-latency reporting).
func markProxyDown(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		if h.currentlyUp {
			h.downSince = time.Now()
		}
		h.currentlyUp = false
	}
}

// UnregisterProxy removes a proxy from the health registry after its goroutine
// has fully drained. Must be called after the goroutine exits, not at cancel time.
func UnregisterProxy(id int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	delete(proxyHealthByIndex, id)
}

// ProxyHealthCount returns the number of registered proxies (0 = non-proxy mode).
func ProxyHealthCount() int {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	return len(proxyHealthByIndex)
}

func formatProxyEntry(index int, address string) string {
	return fmt.Sprintf("proxy[%d] (%s)", index, address)
}

// sortedIndicesLocked returns registry indices in ascending order. Caller holds the lock.
func sortedIndicesLocked() []int {
	indices := make([]int, 0, len(proxyHealthByIndex))
	for idx := range proxyHealthByIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}

// ProxyHealthSnapshot returns the current state without advancing the transition
// baseline, so it is safe to call from the pulse-fire marker. Lists are complete
// (no display cap) and index-sorted.
func ProxyHealthSnapshot() (up int, dead []string, degraded []string, bandwidth map[string]ProxyBandwidth) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	bandwidth = make(map[string]ProxyBandwidth)
	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]
		switch {
		case h.currentlyUp:
			up++
		case h.everUp:
			degraded = append(degraded, formatProxyEntry(idx, h.address))
		default:
			dead = append(dead, formatProxyEntry(idx, h.address))
		}
		
		if h.bw != nil {
			var pb ProxyBandwidth
			pb.TotalRx.Store(h.bw.TotalRx.Load())
			pb.TotalTx.Store(h.bw.TotalTx.Load())
			pb.BillableRx.Store(h.bw.BillableRx.Load())
			pb.BillableTx.Store(h.bw.BillableTx.Load())
			bandwidth[formatProxyEntry(idx, h.address)] = pb
		}
	}
	return up, dead, degraded, bandwidth
}

// ProxyHealthHeartbeat builds the per-heartbeat report and advances the transition
// baseline. Call exactly once per heartbeat. On the first call it only establishes
// the baseline (no transition events). NewlyDead is populated only when confirmDead
// is true (caller passes uptime >= deadConfirmDelay), once per never-up proxy.
func ProxyHealthHeartbeat(confirmDead bool) ProxyHealthReport {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()

	now := time.Now()
	first := !proxyBaselineSet
	var r ProxyHealthReport
	r.Bandwidth = make(map[string]ProxyBandwidth)

	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]

		switch {
		case h.currentlyUp:
			r.Up++
		case h.everUp:
			r.Degraded = append(r.Degraded, formatProxyEntry(idx, h.address))
		default:
			r.Dead = append(r.Dead, formatProxyEntry(idx, h.address))
		}

		if h.bw != nil {
			var pb ProxyBandwidth
			pb.TotalRx.Store(h.bw.TotalRx.Load())
			pb.TotalTx.Store(h.bw.TotalTx.Load())
			pb.BillableRx.Store(h.bw.BillableRx.Load())
			pb.BillableTx.Store(h.bw.BillableTx.Load())
			r.Bandwidth[formatProxyEntry(idx, h.address)] = pb
		}

		if !first {
			switch {
			case h.currentlyUp && !h.lastSeenUp:
				ev := ProxyEvent{Index: idx, Address: h.address}
				if !h.downSince.IsZero() {
					ev.After = now.Sub(h.downSince)
				}
				r.Recovered = append(r.Recovered, ev)
				proxyLifetimeRecovered++
			case !h.currentlyUp && h.lastSeenUp:
				r.NewlyDegraded = append(r.NewlyDegraded, ProxyEvent{Index: idx, Address: h.address})
				proxyLifetimeLost++
			}
		}

		if confirmDead && !h.currentlyUp && !h.everUp && !h.deadLogged {
			r.NewlyDead = append(r.NewlyDead, ProxyEvent{Index: idx, Address: h.address})
			h.deadLogged = true
		}

		h.lastSeenUp = h.currentlyUp
	}

	proxyBaselineSet = true
	r.LifetimeRecovered = proxyLifetimeRecovered
	r.LifetimeLost = proxyLifetimeLost
	return r
}
