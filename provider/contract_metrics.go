package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

const contractBucketSeconds = 10
const contractBucketCount = 960 // covers ~160 min at 10s buckets

type contractBucket struct {
	acquired int64
	denied   int64
}

type proxyContractMetrics struct {
	Acquired atomic.Int64
	Denied   atomic.Int64

	mu          sync.Mutex
	buckets     [contractBucketCount]contractBucket
	bucketPos   int
	bucketEpoch int64
}

func (m *proxyContractMetrics) add(acquired bool) {
	if acquired {
		m.Acquired.Add(1)
	} else {
		m.Denied.Add(1)
	}

	now := time.Now().Unix()
	m.mu.Lock()
	epoch := now / contractBucketSeconds
	if epoch != m.bucketEpoch {
		m.bucketEpoch = epoch
		m.bucketPos = (m.bucketPos + 1) % contractBucketCount
		m.buckets[m.bucketPos] = contractBucket{}
	}
	if acquired {
		m.buckets[m.bucketPos].acquired++
	} else {
		m.buckets[m.bucketPos].denied++
	}
	m.mu.Unlock()
}

func (m *proxyContractMetrics) window(d time.Duration) (acquired, denied int64) {
	cutoff := time.Now().Add(-d).Unix()
	needed := int(d.Seconds() / contractBucketSeconds)
	if needed > contractBucketCount {
		needed = contractBucketCount
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	epoch := m.bucketEpoch
	for i := 0; i < needed; i++ {
		idx := (m.bucketPos - i + contractBucketCount) % contractBucketCount
		bucketEpoch := epoch - int64(i)
		if bucketEpoch*contractBucketSeconds < cutoff {
			break
		}
		acquired += m.buckets[idx].acquired
		denied += m.buckets[idx].denied
	}
	return acquired, denied
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

func registerContractCallback(index int, client *connect.Client) {
	metrics := globalContractMetrics.getOrCreate(index)
	client.ContractManager().AddContractStatusCallback(func(cs *connect.ContractStatus) {
		acquired := cs.Error == nil
		metrics.add(acquired)
	})
}
