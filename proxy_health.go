package connect

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyBandwidth tracks the data usage and session timing of a proxy.
type ProxyBandwidth struct {
	TotalRx, TotalTx, BillableRx, BillableTx atomic.Uint64
	Clients                                  atomic.Int64
	LatencyNs                                atomic.Int64
	SocksLatencyNs                           atomic.Int64

	mu            sync.Mutex
	sessions      map[any]time.Time
	presenceSince time.Time // when clients first arrived in the current continuous window
	lastActivity  time.Time // when the last session was added or removed
}

// clientPresenceGrace is how long after all sessions close before resetting
// the presenceSince clock. Prevents age from resetting to 0s on every short
// flow when a client makes many rapid requests (e.g. browser tabs).
const clientPresenceGrace = 10 * time.Second

func (self *ProxyBandwidth) AddSession(key any, start time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.sessions == nil {
		self.sessions = make(map[any]time.Time)
	}
	if len(self.sessions) == 0 {
		gap := time.Since(self.lastActivity)
		if self.presenceSince.IsZero() || gap >= clientPresenceGrace {
			self.presenceSince = start
		}
	}
	self.sessions[key] = start
	self.lastActivity = time.Now()
}

func (self *ProxyBandwidth) RemoveSession(key any) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.sessions != nil {
		delete(self.sessions, key)
		if len(self.sessions) == 0 {
			self.lastActivity = time.Now()
		}
	}
}

func (self *ProxyBandwidth) MaxAge() time.Duration {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.ageLocked()
}

// ageLocked returns the continuous-presence age WITHOUT mutating state. Caller
// holds self.mu. Previously MaxAge() cleared presenceSince as a side effect,
// which meant a read from the heartbeat/snapshot path silently expired the live
// window — making age depend on read cadence. presenceSince is now reset only by
// AddSession when a new presence window begins (gap >= clientPresenceGrace), so
// this reader is safe to call from snapshots.
func (self *ProxyBandwidth) ageLocked() time.Duration {
	if self.presenceSince.IsZero() {
		return 0
	}
	if len(self.sessions) == 0 && time.Since(self.lastActivity) >= clientPresenceGrace {
		return 0
	}
	return time.Since(self.presenceSince)
}

// ProxyFailureCounters tracks per-proxy failure counts by category, so
// operators can distinguish recurring auth errors from transient timeouts.
type ProxyFailureCounters struct {
	AuthFailures    atomic.Int64
	TimeoutFailures atomic.Int64
	TransportDrops  atomic.Int64
}

// proxyHealth tracks one proxy's platform-transport liveness for the
// [health][proxies] report. See docs/design/dead-proxy-health-report.md.
type proxyHealth struct {
	address     string
	currentlyUp bool
	everUp      bool
	connecting  bool      // registered and still trying to establish first WebSocket
	downSince   time.Time // when currentlyUp last went false (for recovery latency)
	lastSeenUp  bool      // currentlyUp as of the previous heartbeat (baseline)
	deadLogged  bool      // a confirmed-dead event has been emitted for this proxy
	bw          *ProxyBandwidth
	failures    ProxyFailureCounters
	lastError   string
	lastErrorAt time.Time // when lastError was recorded (so a stale error reads as such)
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

	Bandwidth map[string]*ProxyBandwidth
}

var (
	proxyHealthMu      sync.Mutex
	proxyHealthByIndex = map[int]*proxyHealth{}
	// addr -> health, kept in sync with proxyHealthByIndex so ProxyBandwidthByAddress
	// is O(1). It is polled every 5s per draining proxy during hot-reload; a linear
	// scan there was O(proxies) under the global lock for every poll.
	proxyHealthByAddr = map[string]*proxyHealth{}

	proxyLifetimeRecovered int
	proxyLifetimeLost      int
	proxyBaselineSet       bool
)

func RegisterProxy(index int, address string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		h = &proxyHealth{}
		proxyHealthByIndex[index] = h
	}
	if h.address != "" && h.address != address {
		delete(proxyHealthByAddr, h.address)
	}
	h.address = address
	h.connecting = true
	proxyHealthByAddr[address] = h
}

// RegisterProxyBandwidth securely retrieves or initializes the proxyBandwidth.
func RegisterProxyBandwidth(index int) *ProxyBandwidth {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		// Initialize it if it doesn't exist
		h = &proxyHealth{address: "", connecting: true}
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
		h.connecting = false
	}
}

// ProxyBandwidthByAddress returns the ProxyBandwidth for a given address, or nil.
func ProxyBandwidthByAddress(addr string) *ProxyBandwidth {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByAddr[addr]; ok {
		return h.bw
	}
	return nil
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
		h.connecting = false
	}
}

// RecordProxyAuthFailure increments the auth-failure counter for a proxy.
func RecordProxyAuthFailure(index int, err error) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		if strings.Contains(strings.ToLower(errStr), "timeout") {
			h.failures.TimeoutFailures.Add(1)
		} else {
			h.failures.AuthFailures.Add(1)
		}
		h.lastError = errStr
		h.lastErrorAt = time.Now()
	}
}

// RecordProxyTransportDrop increments the transport-drop counter for a proxy.
func RecordProxyTransportDrop(index int, err error) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.failures.TransportDrops.Add(1)
		if err != nil {
			h.lastError = err.Error()
			h.lastErrorAt = time.Now()
		}
	}
}

// UnregisterProxy removes a proxy from the health registry after its goroutine
// has fully drained. Must be called after the goroutine exits, not at cancel time.
func UnregisterProxy(id int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[id]; ok && h.address != "" {
		// only drop the addr entry if it still points at this proxy
		if proxyHealthByAddr[h.address] == h {
			delete(proxyHealthByAddr, h.address)
		}
	}
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

func formatProxyErrorEntry(index int, address string, failures *ProxyFailureCounters, lastError string, lastErrorAt time.Time) string {
	base := formatProxyEntry(index, address)
	var parts []string
	if auth := failures.AuthFailures.Load(); auth > 0 {
		parts = append(parts, fmt.Sprintf("auth:%d", auth))
	}
	if timeout := failures.TimeoutFailures.Load(); timeout > 0 {
		parts = append(parts, fmt.Sprintf("timeout:%d", timeout))
	}
	if drops := failures.TransportDrops.Load(); drops > 0 {
		parts = append(parts, fmt.Sprintf("drops:%d", drops))
	}
	if lastError != "" {
		if !lastErrorAt.IsZero() {
			parts = append(parts, fmt.Sprintf("last_err:%q (%s ago)", lastError, time.Since(lastErrorAt).Round(time.Second)))
		} else {
			parts = append(parts, fmt.Sprintf("last_err:%q", lastError))
		}
	}
	if len(parts) > 0 {
		return fmt.Sprintf("%s [%s]", base, strings.Join(parts, ", "))
	}
	return base
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
func ProxyHealthSnapshot() (up int, dead []string, degraded []string, bandwidth map[string]*ProxyBandwidth, connecting []string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	bandwidth = make(map[string]*ProxyBandwidth)
	total := 0
	bwCount := 0
	for _, idx := range sortedIndicesLocked() {
		total++
		h := proxyHealthByIndex[idx]
		switch {
		case h.currentlyUp:
			up++
		case h.everUp:
			if !h.downSince.IsZero() && time.Since(h.downSince) >= 7*24*time.Hour {
				dead = append(dead, formatProxyEntry(idx, h.address))
			} else {
				degraded = append(degraded, formatProxyEntry(idx, h.address))
			}
		case h.connecting:
			connecting = append(connecting, formatProxyEntry(idx, h.address))
		default:
			dead = append(dead, formatProxyEntry(idx, h.address))
		}

		if h.bw != nil {
			bwCount++
			pb := &ProxyBandwidth{}
			pb.TotalRx.Store(h.bw.TotalRx.Load())
			pb.TotalTx.Store(h.bw.TotalTx.Load())
			pb.BillableRx.Store(h.bw.BillableRx.Load())
			pb.BillableTx.Store(h.bw.BillableTx.Load())
			pb.Clients.Store(h.bw.Clients.Load())
			pb.AddSession("snapshot", time.Now().Add(-h.bw.MaxAge()))
			bandwidth[formatProxyEntry(idx, h.address)] = pb
		}
	}
	return up, dead, degraded, bandwidth, connecting
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
	r.Bandwidth = make(map[string]*ProxyBandwidth)

	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]

		switch {
		case h.currentlyUp:
			r.Up++
		case h.everUp:
			r.Degraded = append(r.Degraded, formatProxyErrorEntry(idx, h.address, &h.failures, h.lastError, h.lastErrorAt))
		case h.connecting:
			// still trying to connect — not dead
		default:
			r.Dead = append(r.Dead, formatProxyErrorEntry(idx, h.address, &h.failures, h.lastError, h.lastErrorAt))
		}

		if h.bw != nil {
			pb := &ProxyBandwidth{}
			pb.TotalRx.Store(h.bw.TotalRx.Load())
			pb.TotalTx.Store(h.bw.TotalTx.Load())
			pb.BillableRx.Store(h.bw.BillableRx.Load())
			pb.BillableTx.Store(h.bw.BillableTx.Load())
			pb.Clients.Store(h.bw.Clients.Load())
			pb.AddSession("snapshot", time.Now().Add(-h.bw.MaxAge()))
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

		// Only mark as "newly dead" if the proxy was NOT connecting
		// (never got a chance = still trying = not dead)
		if confirmDead && !h.currentlyUp && !h.everUp && !h.connecting && !h.deadLogged {
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

// ProxyHealthStatus represents the live health of a proxy
type ProxyHealthStatus struct {
	Health         string
	DownSince      time.Time
	AuthFailures   int64
	TransportDrops int64
	TimeoutFails   int64
	LatencyMs      int64
	SocksLatencyMs int64
}

// ProxyHealthByAddress returns the current health classification for each
// registered proxy, keyed by address. Used to update proxy.state snapshots.
func ProxyHealthByAddress() map[string]ProxyHealthStatus {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	result := make(map[string]ProxyHealthStatus, len(proxyHealthByIndex))
	for _, h := range proxyHealthByIndex {
		health := "dead"
		if h.currentlyUp {
			health = "up"
		} else if h.everUp {
			health = degradedTierFromDuration(time.Since(h.downSince))
		} else if h.connecting {
			health = "connecting"
		}
		latencyNs := int64(0)
		socksLatencyNs := int64(0)
		if h.bw != nil {
			latencyNs = h.bw.LatencyNs.Load()
			socksLatencyNs = h.bw.SocksLatencyNs.Load()
		}
		result[h.address] = ProxyHealthStatus{
			Health:         health,
			DownSince:      h.downSince,
			AuthFailures:   h.failures.AuthFailures.Load(),
			TransportDrops: h.failures.TransportDrops.Load(),
			TimeoutFails:   h.failures.TimeoutFailures.Load(),
			LatencyMs:      latencyNs / 1_000_000,
			SocksLatencyMs: socksLatencyNs / 1_000_000,
		}
	}
	return result
}

func degradedTierFromDuration(d time.Duration) string {
	switch {
	case d < 24*time.Hour:
		return "recently_offline"
	case d < 72*time.Hour:
		return "offline"
	case d < 7*24*time.Hour:
		return "long_offline"
	default:
		return "inactive"
	}
}
