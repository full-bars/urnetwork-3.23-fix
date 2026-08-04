package main

import (
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
//   - coarse: 10min x 144 = 24h, covers the 24h window.
//
// A single ring long enough for 24h would be 8,640 buckets (104 KB per proxy);
// the two-tier split keeps the per-proxy footprint at 420x12 + 144x12 = 6.7 KB.
const contractFineWidth, contractFineCount = 10, 420
const contractCoarseWidth, contractCoarseCount = 600, 144

type proxyContractMetrics struct {
	Acquired atomic.Int64
	Denied   atomic.Int64

	mu     sync.Mutex
	fine   *contractRing // nil until first add, nil once retired
	coarse *contractRing // nil until first add, nil once retired

	unsubscribe func() // contract status callback removal; called on retire
}

func (m *proxyContractMetrics) add(acquired bool) {
	if acquired {
		m.Acquired.Add(1)
	} else {
		m.Denied.Add(1)
	}

	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fine == nil {
		m.fine = newContractRing(contractFineWidth, contractFineCount)
		m.coarse = newContractRing(contractCoarseWidth, contractCoarseCount)
	}
	m.fine.add(now, acquired)
	m.coarse.add(now, acquired)
}

func (m *proxyContractMetrics) window(d time.Duration) (acquired, denied int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.fine
	if d.Seconds() > float64(contractFineWidth*contractFineCount) {
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

// retire releases a proxy's bucket rings while keeping the registry entry and
// lifetime totals. The entry itself must NOT be deleted: liveContractsAcquired
// feeds the degraded-proxy reaper's lifetime-contribution score, and a returning
// proxy reuses the same stable ID — a deleted entry would make a proven good
// proxy look like a zero contributor and a prime reaper candidate.
func (r *contractMetricsRegistry) retire(index int) {
	m := r.get(index)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.fine, m.coarse = nil, nil
	unsubscribe := m.unsubscribe
	m.unsubscribe = nil
	m.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}

func registerContractCallback(index int, client *connect.Client) {
	metrics := globalContractMetrics.getOrCreate(index)
	unsubscribe := client.ContractManager().AddContractStatusCallback(func(cs *connect.ContractStatus) {
		acquired := cs.Error == nil
		metrics.add(acquired)
	})
	metrics.mu.Lock()
	metrics.unsubscribe = unsubscribe
	metrics.mu.Unlock()
}
