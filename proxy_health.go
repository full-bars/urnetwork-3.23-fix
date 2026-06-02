package connect

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// proxyHealth tracks one proxy's platform-transport liveness for the
// [health][proxies] report. See docs/design/dead-proxy-health-report.md.
type proxyHealth struct {
	address     string
	currentlyUp bool
	everUp      bool
	downSince   time.Time // when currentlyUp last went false (for recovery latency)
	lastSeenUp  bool      // currentlyUp as of the previous heartbeat (baseline)
	deadLogged  bool      // a confirmed-dead event has been emitted for this proxy
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
