package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// contractBucket counts contract outcomes within one fixed-width time bucket.
// epoch is the bucket index (unix seconds / widthSeconds); 0 means never written.
type contractBucket struct {
	epoch    int32
	acquired int32
	denied   int32
}

// contractRing is a fixed-width circular buffer of contract counts. Buckets are
// written in strictly increasing epoch order, so walking backwards from pos
// yields strictly decreasing epochs — that is what lets window() stop at the
// first out-of-range bucket instead of scanning the whole ring.
type contractRing struct {
	widthSeconds int64
	buckets      []contractBucket
	pos          int
}

func newContractRing(widthSeconds int64, count int) *contractRing {
	return &contractRing{widthSeconds: widthSeconds, buckets: make([]contractBucket, count)}
}

func (r *contractRing) add(now int64, acquired bool) {
	epoch := int32(now / r.widthSeconds)
	if r.buckets[r.pos].epoch != epoch {
		r.pos = (r.pos + 1) % len(r.buckets)
		r.buckets[r.pos] = contractBucket{epoch: epoch}
	}
	if acquired {
		r.buckets[r.pos].acquired++
	} else {
		r.buckets[r.pos].denied++
	}
}

func (r *contractRing) window(now int64, d time.Duration) (acquired, denied int64) {
	minEpoch := int32((now - int64(d/time.Second)) / r.widthSeconds)
	for i := 0; i < len(r.buckets); i++ {
		b := r.buckets[(r.pos-i+len(r.buckets))%len(r.buckets)]
		if b.epoch == 0 || b.epoch < minEpoch {
			break
		}
		acquired += int64(b.acquired)
		denied += int64(b.denied)
	}
	return acquired, denied
}

// Two rings, so the 24h window is real:
//   - fine: 10s x 420 = 70 min, covers the 15m and 1h windows.
//   - coarse: 10min x 145 = 24h 10min, covers the 24h window.
//
// The coarse ring carries one extra bucket so the inclusive cutoff (the bucket
// straddling minEpoch is counted whole) still fits: a full 24h span can occupy
// 145 distinct epochs (144 whole + the straddling one). A single ring long
// enough for 24h would be 8,640 buckets (104 KB per proxy); the two-tier split
// keeps the per-proxy footprint at 420x12 + 145x12 = 6.78 KB.
const contractFineWidth, contractFineCount = 10, 420
const contractCoarseWidth, contractCoarseCount = 600, 145

type proxyContractMetrics struct {
	Acquired atomic.Int64
	Denied   atomic.Int64

	mu         sync.Mutex
	generation uint32        // bumped on retire; add() only records for the bound generation
	fine       *contractRing // nil until first add, nil once retired
	coarse     *contractRing // nil until first add, nil once retired

	unsubscribe func() // contract status callback removal; cleared on retire
}

// add records one contract outcome for the registration bound to generation.
// A stale callback (fired after retire, or superseded by a replacement
// registration) is dropped: it must not resurrect the rings nor inflate the
// lifetime totals of the current owner. The epoch is sampled under the lock so
// both rings share the post-lock instant.
func (m *proxyContractMetrics) add(acquired bool, generation uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	if acquired {
		m.Acquired.Add(1)
		connect.IncrContractAcquired()
	} else {
		m.Denied.Add(1)
		connect.IncrContractDenied()
	}
	if m.fine == nil {
		m.fine = newContractRing(contractFineWidth, contractFineCount)
		m.coarse = newContractRing(contractCoarseWidth, contractCoarseCount)
	}
	now := time.Now().Unix()
	m.fine.add(now, acquired)
	m.coarse.add(now, acquired)
}

func (m *proxyContractMetrics) window(d time.Duration) (acquired, denied int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.fine
	if d.Seconds() >= float64(contractFineWidth*contractFineCount) {
		ring = m.coarse
	}
	if ring == nil {
		return 0, 0
	}
	return ring.window(time.Now().Unix(), d)
}

func (m *proxyContractMetrics) snapshot() (acquired, denied int64) {
	return m.Acquired.Load(), m.Denied.Load()
}

// retireIfOwner ends the registration bound to ownerGen. If the entry is still
// owned by that registration, the generation is bumped (invalidating its
// callbacks), the rings are released, and the stored unsubscribe is returned.
// If a newer registration owns the entry — a replacement proxy at the same
// stable index — the rings are left alone so the replacement keeps recording.
// The unsubscribe is best-effort cleanup; the callback worker dies with its
// client's context regardless.
func (m *proxyContractMetrics) retireIfOwner(ownerGen uint32) (unsub func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation == ownerGen {
		m.generation++
		m.fine, m.coarse = nil, nil
		unsub = m.unsubscribe
		m.unsubscribe = nil
	}
	return unsub
}

type contractMetricsRegistry struct {
	mu    sync.RWMutex
	items map[int]*proxyContractMetrics
}

var globalContractMetrics = &contractMetricsRegistry{
	items: make(map[int]*proxyContractMetrics),
}

func (r *contractMetricsRegistry) getOrCreate(index int) *proxyContractMetrics {
	r.mu.RLock()
	m, ok := r.items[index]
	r.mu.RUnlock()
	if ok {
		return m
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok = r.items[index]; ok {
		return m
	}
	m = &proxyContractMetrics{}
	r.items[index] = m
	return m
}

func (r *contractMetricsRegistry) get(index int) *proxyContractMetrics {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.items[index]
}

func (r *contractMetricsRegistry) all() map[int]*proxyContractMetrics {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[int]*proxyContractMetrics, len(r.items))
	for k, v := range r.items {
		out[k] = v
	}
	return out
}

func (r *contractMetricsRegistry) totals() (acquired, denied int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.items {
		acquired += m.Acquired.Load()
		denied += m.Denied.Load()
	}
	return acquired, denied
}

func (r *contractMetricsRegistry) windowTotals(d time.Duration) (acquired, denied int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.items {
		a, n := m.window(d)
		acquired += a
		denied += n
	}
	return acquired, denied
}

// registerContractCallback wires a contract-status callback for a proxy spawn
// and returns a func that ends this registration's metrics on teardown. Each
// registration binds its callbacks to the entry generation, so a late retire
// from an older spawn of the same stable index cannot release the replacement's
// rings. Defer the returned func from the same goroutine that registered.
// contractDiscovery is the answer to "why is my provider not finding a data
// path": how many contracts were acquired, how many were refused, and for which
// reason. It counts across every registered client and logs a refusal at most
// once a minute with the number it stands for.
type contractDiscovery struct {
	mu       sync.Mutex
	acquired uint64
	denied   uint64
	reasons  map[string]uint64
	window   int
	lastLog  time.Time
}

var globalContractDiscovery contractDiscovery

func (self *contractDiscovery) record(status *connect.ContractStatus) {
	self.mu.Lock()
	if self.reasons == nil {
		self.reasons = map[string]uint64{}
	}
	now := time.Now()
	if status.Error == nil {
		self.acquired++
		self.mu.Unlock()
		return
	}
	self.denied++
	reason := status.Error.String()
	self.reasons[reason]++
	self.window++
	emit := self.lastLog.IsZero() || time.Minute <= now.Sub(self.lastLog)
	count := self.window
	if emit {
		self.window = 0
		self.lastLog = now
	}
	self.mu.Unlock()
	if !emit {
		return
	}
	if count == 1 {
		tlog("[contracts] a provider refused a contract: %s\n", reason)
		return
	}
	tlog("[contracts] %d contracts refused in the last minute; latest reason: %s\n", count, reason)
}

func (self *contractDiscovery) metricsText() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP urnet_contract_attempts_total Contract acquisition attempts, by result.\n")
	fmt.Fprintf(&b, "# TYPE urnet_contract_attempts_total counter\n")
	fmt.Fprintf(&b, "urnet_contract_attempts_total{result=\"acquired\"} %d\n", self.acquired)
	fmt.Fprintf(&b, "urnet_contract_attempts_total{result=\"denied\"} %d\n", self.denied)
	fmt.Fprintf(&b, "# HELP urnet_contract_denied_total Contract refusals, by the reason the provider gave.\n")
	fmt.Fprintf(&b, "# TYPE urnet_contract_denied_total counter\n")
	reasons := make([]string, 0, len(self.reasons))
	for reason := range self.reasons {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(&b, "urnet_contract_denied_total{reason=%s} %d\n",
			connect.PrometheusLabelValue(reason), self.reasons[reason])
	}
	return b.String()
}

func contractDiscoveryMetricsText() string {
	return globalContractDiscovery.metricsText()
}

func registerContractCallback(index int, client *connect.Client) func() {
	metrics := globalContractMetrics.getOrCreate(index)
	metrics.mu.Lock()
	metrics.generation++
	ownerGen := metrics.generation
	metrics.mu.Unlock()

	unsubscribe := client.ContractManager().AddContractStatusCallback(func(cs *connect.ContractStatus) {
		acquired := cs.Error == nil
		globalContractDiscovery.record(cs)
		metrics.add(acquired, ownerGen)
	})

	metrics.mu.Lock()
	if metrics.generation != ownerGen {
		// retirement won while the callback was being installed: never store it
		metrics.mu.Unlock()
		unsubscribe()
		return func() {}
	}
	metrics.unsubscribe = unsubscribe
	metrics.mu.Unlock()

	return func() {
		if unsub := metrics.retireIfOwner(ownerGen); unsub != nil {
			unsub()
		}
	}
}
