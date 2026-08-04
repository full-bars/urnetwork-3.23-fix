package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
	coarse := newContractRing(600, 145)
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

	// Selection logic: window(d) routes >=70 min to the coarse ring.
	m.mu.Lock()
	m.fine = fine
	m.coarse = coarse
	m.mu.Unlock()
	a, d = m.window(24 * time.Hour)
	if a != 24 || d != 0 {
		t.Fatalf("metrics 24h window = %d acquired / %d denied, want 24/0 (coarse ring not selected)", a, d)
	}
	// Exactly 70 minutes also routes to the coarse ring (the fine ring cannot
	// represent the inclusive cutoff at its own capacity). 7 whole 10-min
	// buckets plus the straddling bucket at the exact cutoff = 8 writes.
	a, d = m.window(70 * time.Minute)
	if a != 8 || d != 0 {
		t.Fatalf("metrics 70m window = %d acquired / %d denied, want 8/0 (coarse ring not selected)", a, d)
	}
}

func TestContractCoarseRingHoldsFull24hSpan(t *testing.T) {
	// A full 24h span can occupy 145 distinct coarse epochs (144 whole buckets
	// plus the bucket straddling the cutoff). The ring must have 145 slots so
	// the inclusive cutoff does not under-count the oldest bucket.
	coarse := newContractRing(600, 145)
	base := time.Now().Unix()
	start := base - 144*600 // 144 buckets back: the oldest whole bucket
	for i := 0; i < 145; i++ {
		coarse.add(start+int64(i)*600, true)
	}
	a, d := coarse.window(base, 24*time.Hour)
	if a != 145 || d != 0 {
		t.Fatalf("coarse 24h window over full span = %d acquired / %d denied, want 145/0", a, d)
	}
}

func TestRetireKeepsLifetimeTotals(t *testing.T) {
	m := &proxyContractMetrics{}
	m.mu.Lock()
	gen := m.generation
	m.mu.Unlock()
	m.add(true, gen)
	m.add(true, gen)
	m.add(false, gen)
	if a, _ := m.snapshot(); a != 2 {
		t.Fatalf("lifetime acquired = %d, want 2", a)
	}

	unsub := m.retireIfOwner(gen)
	if unsub != nil {
		t.Fatal("expected no stored unsubscribe")
	}

	if a, d := m.snapshot(); a != 2 || d != 1 {
		t.Fatalf("lifetime totals after retire = %d/%d, want 2/1", a, d)
	}
	if a, d := m.window(time.Minute); a != 0 || d != 0 {
		t.Fatalf("window after retire = %d/%d, want 0/0 (rings must be nil)", a, d)
	}

	// A stale callback (old generation) must not resurrect the rings or move
	// the lifetime totals of the retired entry.
	m.add(true, gen)
	if a, d := m.snapshot(); a != 2 || d != 1 {
		t.Fatalf("totals after stale add = %d/%d, want 2/1 (obsolete callback recorded)", a, d)
	}
	m.mu.Lock()
	ringsNil := m.fine == nil && m.coarse == nil
	m.mu.Unlock()
	if !ringsNil {
		t.Fatal("stale add resurrected the rings after retire")
	}
}

func TestRetireCallsUnsubscribe(t *testing.T) {
	called := int32(0)
	m := &proxyContractMetrics{}
	m.mu.Lock()
	m.generation++
	gen := m.generation
	m.unsubscribe = func() { atomic.AddInt32(&called, 1) }
	m.mu.Unlock()

	if unsub := m.retireIfOwner(gen); unsub == nil {
		t.Fatal("retire did not return the stored unsubscribe")
	} else {
		unsub()
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("retire did not invoke the stored unsubscribe")
	}
}

func TestRetireDoesNotKillReplacement(t *testing.T) {
	// A respawn reuses the stable index: an older spawn's retire must not
	// release the replacement's rings or invalidate its callbacks.
	m := &proxyContractMetrics{}
	m.mu.Lock()
	m.generation++
	ownerA := m.generation
	m.mu.Unlock()
	m.add(true, ownerA)

	m.mu.Lock()
	m.generation++
	ownerB := m.generation
	m.mu.Unlock()
	m.add(false, ownerB)

	// A's late retire: not the owner anymore, must leave B intact.
	if unsub := m.retireIfOwner(ownerA); unsub != nil {
		t.Fatal("A's retire returned an unsubscribe it does not own")
	}
	m.add(true, ownerB)
	if a, d := m.snapshot(); a != 2 || d != 1 {
		t.Fatalf("totals after A's late retire = %d/%d, want 2/1", a, d)
	}
	m.mu.Lock()
	ringsIntact := m.fine != nil && m.coarse != nil
	m.mu.Unlock()
	if !ringsIntact {
		t.Fatal("A's late retire released the replacement's rings")
	}

	// B's own retire works normally afterwards.
	if unsub := m.retireIfOwner(ownerB); unsub != nil {
		t.Fatal("B's retire returned a stored unsubscribe unexpectedly")
	}
	if a, d := m.window(time.Minute); a != 0 || d != 0 {
		t.Fatalf("window after B's retire = %d/%d, want 0/0", a, d)
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
			m.mu.Lock()
			gen := m.generation
			m.mu.Unlock()
			for j := 0; j < 200; j++ {
				m.add(j%2 == 0, gen)
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
			m := r.getOrCreate(idx)
			m.mu.Lock()
			gen := m.generation
			m.mu.Unlock()
			m.retireIfOwner(gen)
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

// newTestConnectClient builds a minimal *connect.Client suitable for wiring a
// real ContractManager without any network I/O, mirroring the pattern used by
// the connect package's own tests (transfer_contract_manager_test.go).
func newTestConnectClient(t *testing.T) *connect.Client {
	t.Helper()
	client := connect.NewClient(context.Background(), connect.NewId(), connect.NewNoContractClientOob(), connect.DefaultClientSettings())
	t.Cleanup(client.Cancel)
	return client
}

func TestRegisterContractCallbackWiresAndRetires(t *testing.T) {
	// Use indices far outside any real proxy range so this test cannot collide
	// with other tests sharing the global registry.
	const index = -900001
	client := newTestConnectClient(t)

	teardown := registerContractCallback(index, client)
	if teardown == nil {
		t.Fatal("registerContractCallback returned a nil teardown func")
	}

	m := globalContractMetrics.get(index)
	if m == nil {
		t.Fatal("registerContractCallback did not create a metrics entry")
	}
	m.mu.Lock()
	ownerGen := m.generation
	hasUnsub := m.unsubscribe != nil
	m.mu.Unlock()
	if ownerGen == 0 {
		t.Fatal("registerContractCallback did not bump the generation")
	}
	if !hasUnsub {
		t.Fatal("registerContractCallback did not store the callback's unsubscribe")
	}

	teardown()

	m.mu.Lock()
	genAfter := m.generation
	unsubAfter := m.unsubscribe
	ringsNil := m.fine == nil && m.coarse == nil
	m.mu.Unlock()
	if genAfter != ownerGen+1 {
		t.Fatalf("generation after teardown = %d, want %d (retire must bump it)", genAfter, ownerGen+1)
	}
	if unsubAfter != nil {
		t.Fatal("teardown did not clear the stored unsubscribe")
	}
	if !ringsNil {
		t.Fatal("teardown did not release the rings")
	}

	// A stale callback bound to the old generation must be dropped, not
	// resurrect the entry.
	m.add(true, ownerGen)
	if a, d := m.snapshot(); a != 0 || d != 0 {
		t.Fatalf("totals after stale add post-teardown = %d/%d, want 0/0", a, d)
	}

	// Calling teardown a second time must be a harmless no-op: the entry is no
	// longer owned by ownerGen, so retireIfOwner must not fire again.
	teardown()
	m.mu.Lock()
	genAfterSecond := m.generation
	m.mu.Unlock()
	if genAfterSecond != genAfter {
		t.Fatalf("second teardown call changed generation from %d to %d, want no-op", genAfter, genAfterSecond)
	}
}

func TestRegisterContractCallbackRespawnSurvivesOldTeardown(t *testing.T) {
	// Simulates a proxy respawn at the same stable index: the old spawn's
	// deferred teardown must not tear down the replacement's registration.
	const index = -900002
	clientA := newTestConnectClient(t)
	clientB := newTestConnectClient(t)

	teardownA := registerContractCallback(index, clientA)
	m := globalContractMetrics.get(index)
	if m == nil {
		t.Fatal("registerContractCallback did not create a metrics entry")
	}
	m.mu.Lock()
	genA := m.generation
	m.mu.Unlock()

	teardownB := registerContractCallback(index, clientB)
	m.mu.Lock()
	genB := m.generation
	m.mu.Unlock()
	if genB != genA+1 {
		t.Fatalf("respawn generation = %d, want %d (one bump per registration)", genB, genA+1)
	}

	// The old spawn's teardown fires after the respawn: it must leave the
	// replacement's ownership and rings untouched.
	teardownA()
	m.mu.Lock()
	genAfterOldTeardown := m.generation
	m.mu.Unlock()
	if genAfterOldTeardown != genB {
		t.Fatalf("old spawn's teardown changed generation to %d, want unchanged %d", genAfterOldTeardown, genB)
	}

	// The replacement's registration is still live: it can still record.
	m.add(true, genB)
	if a, _ := m.snapshot(); a != 1 {
		t.Fatalf("acquired after old teardown = %d, want 1 (replacement's registration was killed)", a)
	}

	// The replacement's own teardown retires it normally afterwards.
	teardownB()
	m.mu.Lock()
	genAfterNewTeardown := m.generation
	ringsNil := m.fine == nil && m.coarse == nil
	m.mu.Unlock()
	if genAfterNewTeardown != genB+1 {
		t.Fatalf("generation after replacement's teardown = %d, want %d", genAfterNewTeardown, genB+1)
	}
	if !ringsNil {
		t.Fatal("replacement's teardown did not release the rings")
	}
}

func TestRegisterContractCallbackDistinctIndicesAreIndependent(t *testing.T) {
	// Two proxies at different stable indices must never share a metrics
	// entry or interfere with each other's lifecycle.
	const indexX, indexY = -900003, -900004
	clientX := newTestConnectClient(t)
	clientY := newTestConnectClient(t)

	teardownX := registerContractCallback(indexX, clientX)
	teardownY := registerContractCallback(indexY, clientY)
	defer teardownY()

	mx := globalContractMetrics.get(indexX)
	my := globalContractMetrics.get(indexY)
	if mx == nil || my == nil {
		t.Fatal("registerContractCallback did not create metrics entries for both indices")
	}
	if mx == my {
		t.Fatal("distinct indices share the same metrics entry")
	}

	teardownX()

	mx.mu.Lock()
	xRingsNil := mx.fine == nil && mx.coarse == nil
	mx.mu.Unlock()
	if !xRingsNil {
		t.Fatal("index X teardown did not release its rings")
	}

	my.mu.Lock()
	yGen := my.generation
	my.mu.Unlock()
	my.add(true, yGen)
	if a, _ := my.snapshot(); a != 1 {
		t.Fatalf("index Y acquired = %d, want 1 (unaffected by index X's teardown)", a)
	}
}
