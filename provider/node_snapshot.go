package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// NodeSnapshot is the typed picture of the node served by the control socket
// "snapshot" command. Its JSON shape is the wire contract with urnet-tools,
// pinned by the fixtures in testdata/livestatus. Fields are only ever added.
// Optional fields are omitted when unknown, never zeroed.
type NodeSnapshot struct {
	V               int       `json:"v"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	Now             time.Time `json:"now"`
	UptimeSeconds   float64   `json:"uptime_seconds"`

	// State is one of starting, degraded, idle, flowing.
	State          string `json:"state"`
	Busy           bool   `json:"busy"`
	RestartPending bool   `json:"restart_pending"`

	Rate     SnapshotRate     `json:"rate"`
	Clients  int64            `json:"clients"`
	Sessions SnapshotSessions `json:"sessions"`
	Proxies  SnapshotProxies  `json:"proxies"`
	Pressure float64          `json:"pressure"`
	Restart  SnapshotRestart  `json:"restart"`

	Resources SnapshotResources `json:"resources"`

	// IdleHint says why the node is idle. Set only when State is idle.
	IdleHint string `json:"idle_hint,omitempty"`
}

// SnapshotRate is billable throughput in bytes per second. HistoryBps holds
// per-second samples, oldest first, at most snapshotRingSize of them.
type SnapshotRate struct {
	NowBps          int64   `json:"now_bps"`
	Avg1mBps        int64   `json:"avg_1m_bps"`
	Avg5mBps        int64   `json:"avg_5m_bps"`
	HistoryInterval int     `json:"history_interval_seconds"`
	HistoryBps      []int64 `json:"history_bps"`
}

type SnapshotSessions struct {
	PQE       int64 `json:"pqe"`
	Classical int64 `json:"classical"`
}

type SnapshotProxies struct {
	Up         int `json:"up"`
	Degraded   int `json:"degraded"`
	Connecting int `json:"connecting"`
	Dead       int `json:"dead"`
}

type SnapshotRestart struct {
	Reason        string `json:"reason"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

// SnapshotResources fields the platform cannot supply are left zero and
// omitted from the JSON.
type SnapshotResources struct {
	HeapInuseBytes uint64 `json:"heap_inuse_bytes"`
	MemLimitBytes  uint64 `json:"mem_limit_bytes,omitempty"`
	RSSBytes       uint64 `json:"rss_bytes,omitempty"`
	Goroutines     int    `json:"goroutines"`
	OpenFDs        uint64 `json:"open_fds,omitempty"`
	FDLimit        uint64 `json:"fd_limit,omitempty"`
}

const (
	snapshotSchemaVersion = 1

	// snapshotRingSize is 10 minutes of one-second rate samples.
	snapshotRingSize = 600

	// snapshotCacheTTL is how long Get() reuses a built snapshot.
	snapshotCacheTTL = time.Second

	// snapshotStartingWindow is how long after start the state reads
	// "starting" regardless of traffic.
	snapshotStartingWindow = 120 * time.Second

	// snapshotIdleBps is the 1 minute rate below which the node is idle. It
	// is the same 5 KiB/s default `urnet-tools update --idle` uses.
	snapshotIdleBps = 5120

	// snapshotHistoryInterval and snapshotHistorySize keep 10 minutes of
	// cumulative counter samples for the "in the last 10 min" idle hints.
	snapshotHistoryInterval = 10 * time.Second
	snapshotHistorySize     = 60
)

// rateSampler turns cumulative per-proxy billable byte counters into a
// per-second rate ring. It uses the same delta method as runBillableRateWriter:
// keys present in both samples contribute their delta, new and removed keys
// are ignored, and a counter that went down (a proxy respawn hands back a
// zeroed ProxyBandwidth) contributes nothing for that tick.
type rateSampler struct {
	mu       sync.Mutex
	prev     map[string]uint64
	prevTime time.Time
	ring     [snapshotRingSize]int64
	next     int // slot the next sample is written to
	count    int // valid samples, capped at snapshotRingSize
}

func newRateSampler() *rateSampler {
	return &rateSampler{}
}

// sample records one tick. cur maps proxy key to cumulative billable bytes
// (rx+tx). The first call only establishes the baseline and records nothing.
func (s *rateSampler) sample(cur map[string]uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first := s.prev == nil
	var delta uint64
	for k, v := range cur {
		if p, ok := s.prev[k]; ok && v >= p {
			delta += v - p
		}
	}
	next := make(map[string]uint64, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	elapsed := now.Sub(s.prevTime).Seconds()
	s.prev = next
	s.prevTime = now
	if first {
		return
	}
	if elapsed < 1 {
		elapsed = 1
	}
	s.ring[s.next] = int64(float64(delta) / elapsed)
	s.next = (s.next + 1) % snapshotRingSize
	if s.count < snapshotRingSize {
		s.count++
	}
}

// history returns the samples oldest first. Never nil.
func (s *rateSampler) history() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, s.count)
	start := (s.next - s.count + snapshotRingSize) % snapshotRingSize
	for i := 0; i < s.count; i++ {
		out[i] = s.ring[(start+i)%snapshotRingSize]
	}
	return out
}

// rates returns the newest sample and the averages over the last 60 and 300
// samples, or over however many exist yet.
func (s *rateSampler) rates() (now, avg1m, avg5m int64) {
	h := s.history()
	if len(h) == 0 {
		return 0, 0, 0
	}
	return h[len(h)-1], avgTail(h, 60), avgTail(h, 300)
}

func avgTail(h []int64, n int) int64 {
	if len(h) < n {
		n = len(h)
	}
	if n == 0 {
		return 0
	}
	var sum int64
	for _, v := range h[len(h)-n:] {
		sum += v
	}
	return sum / int64(n)
}

// cumulativeSample is one reading of the monotonic counters behind the idle
// hints.
type cumulativeSample struct {
	at        time.Time
	auth      int64 // sum of per-proxy auth failures (urnet_proxy_auth_failures)
	contracts int64 // contracts acquired (urnet_contracts_total{result="acquired"})
}

// cumulativeHistory keeps a small ring of counter samples, one per
// snapshotHistoryInterval, so "in the last 10 minutes" questions have data.
type cumulativeHistory struct {
	mu      sync.Mutex
	samples []cumulativeSample
}

// due reports whether enough time passed since the last sample to take
// another. Callers check this before paying for the reads.
func (h *cumulativeHistory) due(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return true
	}
	return now.Sub(h.samples[len(h.samples)-1].at) >= snapshotHistoryInterval
}

func (h *cumulativeHistory) add(s cumulativeSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, s)
	if len(h.samples) > snapshotHistorySize {
		h.samples = append(h.samples[:0], h.samples[len(h.samples)-snapshotHistorySize:]...)
	}
}

func (h *cumulativeHistory) copy() []cumulativeSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]cumulativeSample(nil), h.samples...)
}

// authFailingSince returns when auth failures were first seen to increase
// inside the history window. A decrease (a proxy was removed) is not an
// increase, so only consecutive pairs that went up count.
func authFailingSince(hist []cumulativeSample) (time.Time, bool) {
	for i := 1; i < len(hist); i++ {
		if hist[i].auth > hist[i-1].auth {
			return hist[i].at, true
		}
	}
	return time.Time{}, false
}

// contractsAcquiredInWindow reports whether any contract was acquired across
// the history window.
func contractsAcquiredInWindow(hist []cumulativeSample) bool {
	for i := 1; i < len(hist); i++ {
		if hist[i].contracts > hist[i-1].contracts {
			return true
		}
	}
	return false
}

// stateInputs is everything deriveSnapshotState needs.
type stateInputs struct {
	uptime   time.Duration
	proxies  SnapshotProxies
	pressure float64
	avg1m    int64
}

func (p SnapshotProxies) total() int {
	return p.Up + p.Degraded + p.Connecting + p.Dead
}

// deriveSnapshotState returns starting, degraded, idle or flowing, checked in
// that order.
func deriveSnapshotState(in stateInputs) string {
	if in.uptime < snapshotStartingWindow {
		return "starting"
	}
	total := in.proxies.total()
	if total > 0 && 2*(in.proxies.Dead+in.proxies.Degraded) > total {
		return "degraded"
	}
	if in.pressure >= 0.8 {
		return "degraded"
	}
	if in.avg1m < snapshotIdleBps {
		return "idle"
	}
	return "flowing"
}

// deriveIdleHint picks the first matching reason a node is idle.
func deriveIdleHint(proxies SnapshotProxies, hist []cumulativeSample, now time.Time) string {
	total := proxies.total()
	switch {
	case total == 0:
		return "no proxies configured"
	case proxies.Up == 0:
		return fmt.Sprintf("all %d proxies dead or connecting", total)
	}
	if since, ok := authFailingSince(hist); ok {
		mins := int(now.Sub(since).Minutes())
		if mins < 1 {
			mins = 1
		}
		return fmt.Sprintf("auth failing for %d min", mins)
	}
	if !contractsAcquiredInWindow(hist) {
		return "no contracts acquired in the last 10 min"
	}
	return "no traffic offered"
}

// snapshotSources are the live inputs behind a snapshot, injectable so the
// collector can be tested without a running provider.
type snapshotSources struct {
	now        func() time.Time
	startedAt  time.Time
	billable   func() map[string]uint64 // proxy key to cumulative billable bytes
	pool       func() (SnapshotProxies, int64)
	cumulative func() (auth, contracts int64)
	sessions   func() (pqe, classical int64)
	pressure   func() float64
	version    func() string
	prevVer    func() string
	restart    func() SnapshotRestart
	resources  func() SnapshotResources

	restartPending func() bool
}

// nodeSnapshotCollector samples the node once a second and builds snapshots.
type nodeSnapshotCollector struct {
	src  snapshotSources
	rate *rateSampler
	hist *cumulativeHistory

	mu       sync.Mutex
	cached   *NodeSnapshot
	cachedAt time.Time
}

func newNodeSnapshotCollector(src snapshotSources) *nodeSnapshotCollector {
	return &nodeSnapshotCollector{src: src, rate: newRateSampler(), hist: &cumulativeHistory{}}
}

// tick takes one rate sample and, when due, one counter sample.
func (c *nodeSnapshotCollector) tick() {
	now := c.src.now()
	c.rate.sample(c.src.billable(), now)
	if c.hist.due(now) {
		auth, contracts := c.src.cumulative()
		c.hist.add(cumulativeSample{at: now, auth: auth, contracts: contracts})
	}
}

// run ticks once a second until ctx is done.
func (c *nodeSnapshotCollector) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	c.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick()
		}
	}
}

// Get returns the current snapshot, reusing one built within the last second.
// The result is shared: callers must not modify it.
func (c *nodeSnapshotCollector) Get() *NodeSnapshot {
	now := c.src.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && now.Sub(c.cachedAt) < snapshotCacheTTL && !now.Before(c.cachedAt) {
		return c.cached
	}
	c.cached = c.build(now)
	c.cachedAt = now
	return c.cached
}

func (c *nodeSnapshotCollector) build(now time.Time) *NodeSnapshot {
	nowBps, avg1m, avg5m := c.rate.rates()
	proxies, clients := c.src.pool()
	pqe, classical := c.src.sessions()
	uptime := now.Sub(c.src.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	pressure := c.src.pressure()

	snap := &NodeSnapshot{
		V:               snapshotSchemaVersion,
		Version:         c.src.version(),
		PreviousVersion: c.src.prevVer(),
		StartedAt:       c.src.startedAt.UTC(),
		Now:             now.UTC(),
		UptimeSeconds:   uptime.Seconds(),
		Busy:            avg1m >= snapshotIdleBps,
		RestartPending:  c.src.restartPending(),
		Rate: SnapshotRate{
			NowBps:          nowBps,
			Avg1mBps:        avg1m,
			Avg5mBps:        avg5m,
			HistoryInterval: 1,
			HistoryBps:      c.rate.history(),
		},
		Clients:   clients,
		Sessions:  SnapshotSessions{PQE: pqe, Classical: classical},
		Proxies:   proxies,
		Pressure:  pressure,
		Restart:   c.src.restart(),
		Resources: c.src.resources(),
	}
	snap.State = deriveSnapshotState(stateInputs{uptime: uptime, proxies: proxies, pressure: pressure, avg1m: avg1m})
	if snap.State == "idle" {
		snap.IdleHint = deriveIdleHint(proxies, c.hist.copy(), now)
	}
	return snap
}

// restartPendingFor reports whether any restart-required control key now
// holds a value different from the one the process started with. Keys are
// the ones startupValues reports. A key with no current value is not counted:
// the same absence is what an env-provided setting looks like.
func restartPendingFor(current func(key string) (string, bool), startup map[string]string) bool {
	for key, started := range startup {
		cur, ok := current(key)
		if !ok {
			continue
		}
		if key == "ramlogs" {
			if snapshotTruthy(cur) != snapshotTruthy(started) {
				return true
			}
			continue
		}
		if cur != started {
			return true
		}
	}
	return false
}

func snapshotTruthy(v string) bool {
	switch v {
	case "1", "on", "true", "yes", "ON", "TRUE", "YES", "True", "On", "Yes":
		return true
	}
	return false
}

// --- production wiring ---

var nodeSnapshots = newNodeSnapshotCollector(productionSnapshotSources())

func productionSnapshotSources() snapshotSources {
	return snapshotSources{
		now:       time.Now,
		startedAt: providerStartTime,
		billable: func() map[string]uint64 {
			if connect.ProxyHealthCount() == 0 {
				return map[string]uint64{}
			}
			_, _, _, bw, _ := connect.ProxyHealthSnapshot()
			out := make(map[string]uint64, len(bw))
			for k, p := range bw {
				out[k] = p.BillableRx.Load() + p.BillableTx.Load()
			}
			return out
		},
		pool: func() (SnapshotProxies, int64) {
			up, dead, degraded, bw, connecting := connect.ProxyHealthSnapshot()
			var clients int64
			for _, p := range bw {
				clients += p.Clients.Load()
			}
			return SnapshotProxies{Up: up, Degraded: len(degraded), Connecting: len(connecting), Dead: len(dead)}, clients
		},
		cumulative: func() (int64, int64) {
			return totalProxyAuthFailures(), connect.ContractsAcquiredTotal()
		},
		sessions: func() (int64, int64) {
			pq := pqeTotalCounts()
			return int64(pq.ActivePQE), int64(pq.ActiveClas)
		},
		pressure: currentPressure,
		version: func() string {
			// A build without -ldflags has no version; say "dev" as the
			// control socket "version" command does.
			if v := RequireVersion(); v != "" {
				return v
			}
			return "dev"
		},
		prevVer: func() string {
			startupDiag.mu.Lock()
			defer startupDiag.mu.Unlock()
			return startupDiag.previousVersion
		},
		restart: func() SnapshotRestart {
			detectStartup()
			startupDiag.mu.Lock()
			defer startupDiag.mu.Unlock()
			return SnapshotRestart{Reason: startupDiag.restartReason, CleanShutdown: startupDiag.cleanShutdown}
		},
		resources: collectResources,
		restartPending: func() bool {
			return restartPendingFor(globalControlState.get, startupValues())
		},
	}
}

// totalProxyAuthFailures is the sum behind urnet_proxy_auth_failures.
func totalProxyAuthFailures() int64 {
	state, err := readProxyState()
	if err != nil || state == nil {
		return 0
	}
	var total int64
	for _, entry := range state.Proxies {
		total += entry.AuthFailures
	}
	return total
}

// runNodeSnapshotSampler feeds the global collector until ctx is done.
func runNodeSnapshotSampler(ctx context.Context) {
	nodeSnapshots.run(ctx)
}
