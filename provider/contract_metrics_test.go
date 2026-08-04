package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestContractRingGapDoesNotLeakStaleCounts(t *testing.T) {
	// Regression for the epoch-gap corruption: a proxy that idles for 30 epochs
	// must not report its stale counts inside a fresh 60s window.
	r := newContractRing(10, 420)
	now := int64(1000) // epoch 100
	r.add(now, true)
	now += 300 // jump 30 epochs (5 minutes idle)
	r.add(now, false)

	a, d := r.window(now+60, 60*time.Second)
	if a != 0 || d != 1 {
		t.Fatalf("window after gap = %d acquired / %d denied, want 0/1 (stale add leaked)", a, d)
	}
}

func TestContractRingWindowBoundary(t *testing.T) {
	r := newContractRing(10, 420)
	now := int64(10000)
	// Writes must arrive chronologically (the ring assumes increasing epochs).
	// Outside the window: now-40s (epoch 996).
	r.add(now-40, false)
	// Just inside the cutoff: window(now, 30s) covers epoch (now-30)/10 and later.
	r.add(now-25, true)
	// Inside the window: now-10s.
	r.add(now-10, true)

	a, d := r.window(now, 30*time.Second)
	if a != 2 || d != 0 {
		t.Fatalf("window 30s = %d acquired / %d denied, want 2/0 (bucket straddling cutoff counted whole)", a, d)
	}
}

func TestContractRingWrap(t *testing.T) {
	r := newContractRing(10, 8)
	now := int64(0)
	for i := 0; i < 20; i++ {
		r.add(now, true)
		now += 10 // one epoch per add, 20 epochs > 8 buckets
	}

	// The ring retains only its capacity: data older than 8 buckets is gone.
	a, d := r.window(now, 24*time.Hour)
	if a != 8 || d != 0 {
		t.Fatalf("window after wrap = %d acquired / %d denied, want 8/0 (capacity not enforced)", a, d)
	}

	// The most recent data is intact: a 10s window sees only the last add.
	a, d = r.window(now, 10*time.Second)
	if a != 1 || d != 0 {
		t.Fatalf("window 10s after wrap = %d acquired / %d denied, want 1/0", a, d)
	}
}

func TestContractWindow24hUsesCoarseRing(t *testing.T) {
	// Spread adds over >160 min (past the old single-ring horizon) and confirm
	// the 24h window sees everything via the coarse ring, while the 1h window
	// stays on the fine ring.
	m := &proxyContractMetrics{}

	// Build the deterministic scenario at ring level: writes spread over 4h,
	// anchored to the wall clock so the metrics-level window (which reads
	// time.Now()) sees the same data.
	fine := newContractRing(10, 420)
	coarse := newContractRing(600, 144)
	base := time.Now().Unix()
	start := base - 23*600 // last write lands at ~now
	for i := 0; i < 24; i++ {
		ts := start + int64(i)*600 // every 10 min
		coarse.add(ts, true)
		fine.add(ts, true)
	}

	// 24h window on the coarse ring covers the whole 4h spread.
	a, d := coarse.window(base, 24*time.Hour)
	if a != 24 || d != 0 {
		t.Fatalf("coarse 24h window = %d acquired / %d denied, want 24/0", a, d)
	}
	// A 1h window on the fine ring sees the last 7 writes: the bucket that
	// straddles the cutoff counts whole (documented over-count of up to one
	// bucket width), i.e. writes i=17..23 of the 24.
	a, _ = fine.window(base, 1*time.Hour)
	if a != 7 {
		t.Fatalf("fine 1h window = %d acquired, want 7", a)
	}

	// Selection logic: window(d) routes long durations to the coarse ring.
	m.mu.Lock()
	m.fine = fine
	m.coarse = coarse
	m.mu.Unlock()
	a, d = m.window(24 * time.Hour)
	if a != 24 || d != 0 {
		t.Fatalf("metrics 24h window = %d acquired / %d denied, want 24/0 (coarse ring not selected)", a, d)
	}
}

func TestRetireKeepsLifetimeTotals(t *testing.T) {
	m := &proxyContractMetrics{}
	m.add(true)
	m.add(true)
	m.add(false)
	if a, _ := m.snapshot(); a != 2 {
		t.Fatalf("lifetime acquired = %d, want 2", a)
	}

	r := &contractMetricsRegistry{items: map[int]*proxyContractMetrics{7: m}}
	r.retire(7)

	got := r.get(7)
	if got == nil {
		t.Fatal("retire deleted the registry entry")
	}
	if a, d := got.snapshot(); a != 2 || d != 1 {
		t.Fatalf("lifetime totals after retire = %d/%d, want 2/1", a, d)
	}
	if a, d := got.window(time.Minute); a != 0 || d != 0 {
		t.Fatalf("window after retire = %d/%d, want 0/0 (rings must be nil)", a, d)
	}
}

func TestRetireCallsUnsubscribe(t *testing.T) {
	called := int32(0)
	m := &proxyContractMetrics{}
	m.mu.Lock()
	m.unsubscribe = func() { atomic.AddInt32(&called, 1) }
	m.mu.Unlock()

	r := &contractMetricsRegistry{items: map[int]*proxyContractMetrics{9: m}}
	r.retire(9)
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("retire did not invoke the stored unsubscribe")
	}
}

func TestRegistryConcurrentAddAndRetire(t *testing.T) {
	r := &contractMetricsRegistry{items: map[int]*proxyContractMetrics{}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			m := r.getOrCreate(idx)
			for j := 0; j < 200; j++ {
				m.add(j%2 == 0)
				if j%50 == 0 {
					m.window(15 * time.Minute)
				}
			}
		}(i % 4)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r.retire(idx)
			r.windowTotals(time.Hour)
		}(i)
	}
	wg.Wait()

	for i := 0; i < 4; i++ {
		if r.get(i) == nil {
			t.Fatalf("entry %d vanished under concurrency", i)
		}
	}
}
