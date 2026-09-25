package main

import (
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"
)

const goroutineFixture = `goroutine profile: total 108
90 @ 0x43a1b6 0x4092ad 0x409277 0x7a1b2c 0x468fa1
#	0x7a1b2b	github.com/urnetwork/connect.(*Client).run+0x1b	/src/client.go:210

12 @ 0x43a1b6 0x44b2c5 0x4a1000 0x468fa1
#	0x43a1b5	runtime.gopark+0x15	/go/src/runtime/proc.go:402
#	0x44b2c4	runtime.selectgo+0x4c4	/go/src/runtime/select.go:327
#	0x4a0fff	github.com/urnetwork/connect.(*Transfer).loop+0x9f	/src/transfer.go:88

5 @ 0x43a1b6 0x44b2c5
#	0x43a1b5	runtime.gopark+0x15	/go/src/runtime/proc.go:402
#	0x44b2c4	runtime.selectgo+0x4c4	/go/src/runtime/select.go:327

1 @ 0x43a1b6 0x4a2000
#	0x43a1b5	runtime.gopark+0x15	/go/src/runtime/proc.go:402
#	0x4a1fff	github.com/urnetwork/connect.(*Client).run+0x1b	/src/client.go:210
`

func TestParseGoroutineProfileGroupsByFirstNonRuntimeFrame(t *testing.T) {
	g := parseGoroutineProfile(goroutineFixture, 10)
	if g.Total != 108 {
		t.Fatalf("total = %d, want 108", g.Total)
	}
	if len(g.Groups) != 3 {
		t.Fatalf("groups = %+v, want 3", g.Groups)
	}
	// 90 + 1 land on Client.run, 12 on Transfer.loop, 5 (all runtime) on the
	// very top frame.
	want := []GoroutineGroup{
		{"github.com/urnetwork/connect.(*Client).run", 91},
		{"github.com/urnetwork/connect.(*Transfer).loop", 12},
		{"runtime.gopark", 5},
	}
	for i, w := range want {
		if g.Groups[i] != w {
			t.Errorf("group %d = %+v, want %+v", i, g.Groups[i], w)
		}
	}
}

func TestParseGoroutineProfileLabelsLineIsNotAFrame(t *testing.T) {
	// pprof debug=1 emits "# labels: {\"k\":\"v\"}" between a header and
	// its frames (the labels come from pprof.Do / runtime.SetProfLabel).
	// It must be skipped, not turned into a group name.
	g := parseGoroutineProfile(`goroutine profile: total 91
91 @ 0x43a1b6 0x4092ad
# labels: {"region":"us-west","shard":"4"}
#	0x7a1b2b	github.com/urnetwork/connect.(*Client).run+0x1b	/src/client.go:210
`, 10)
	if g.Total != 91 {
		t.Fatalf("total = %d, want 91", g.Total)
	}
	if len(g.Groups) != 1 {
		t.Fatalf("groups = %+v, want a single Client.run group", g.Groups)
	}
	if g.Groups[0].Func != "github.com/urnetwork/connect.(*Client).run" || g.Groups[0].Count != 91 {
		t.Fatalf("group = %+v, want Client.run 91", g.Groups[0])
	}
}

func TestParseGoroutineProfileGroupsUnderBlockedInIOFrame(t *testing.T) {
	// A goroutine parked in io.ReadFull (via bufio or a TLS read) should
	// group under the connect frame that called it, not under the runtime
	// frame it happens to be blocked in.
	g := parseGoroutineProfile(`goroutine profile: total 20
20 @ 0x43a1b6 0x44b2c5 0x4a1000 0x468fa1 0x7a1b2b
#	0x43a1b5	runtime.gopark+0x15	/go/src/runtime/proc.go:402
#	0x44b2c4	runtime.netpollblock+0x1c4	/go/src/runtime/netpoll.go:520
#	0x4a0fff	internal/poll.runtime_pollWait+0x9f	/go/src/internal/poll/fd_poll_runtime.go:122
#	0x4a1fff	bufio.(*Reader).fill+0x4f	/usr/local/go/src/bufio/bufio.go:110
#	0x7a1b2b	github.com/urnetwork/connect.(*Client).read+0x1b	/src/client.go:210
`, 10)
	if len(g.Groups) != 1 {
		t.Fatalf("groups = %+v, want a single group", g.Groups)
	}
	if g.Groups[0].Func != "github.com/urnetwork/connect.(*Client).read" || g.Groups[0].Count != 20 {
		t.Fatalf("group = %+v, want Client.read 20", g.Groups[0])
	}
}

func TestParseGoroutineProfileLimit(t *testing.T) {
	g := parseGoroutineProfile(goroutineFixture, 1)
	if len(g.Groups) != 1 || g.Groups[0].Count != 91 {
		t.Fatalf("limit 1 = %+v", g.Groups)
	}
	if g.Total != 108 {
		t.Fatalf("total must cover the cut groups too, got %d", g.Total)
	}
}

func TestParseGoroutineProfileEmpty(t *testing.T) {
	if g := parseGoroutineProfile("", 5); g.Total != 0 || len(g.Groups) != 0 {
		t.Fatalf("empty = %+v", g)
	}
}

func rtHist(counts []uint64, buckets []float64) *metrics.Float64Histogram {
	return &metrics.Float64Histogram{Counts: counts, Buckets: buckets}
}

func TestHistDeltaQuantile(t *testing.T) {
	buckets := []float64{0, 0.001, 0.01, 0.1, 1}
	prev := rtHist([]uint64{100, 0, 0, 0}, buckets)
	// 99 more in the first bucket, 1 in the third: p99 is still bucket one.
	cur := rtHist([]uint64{199, 0, 1, 0}, buckets)
	if got := histDeltaQuantile(prev, cur, 0.99); got != 0.001 {
		t.Errorf("p99 = %v, want 0.001", got)
	}
	// Ten more, half of them slow: p99 lands in the slow bucket. The lifetime
	// histogram alone would still say fast, which is why deltas are used.
	cur = rtHist([]uint64{105, 0, 6, 0}, buckets)
	if got := histDeltaQuantile(prev, cur, 0.99); got != 0.1 {
		t.Errorf("p99 = %v, want 0.1", got)
	}
}

func TestHistDeltaQuantileEdges(t *testing.T) {
	buckets := []float64{0, 0.001, 0.01}
	same := rtHist([]uint64{5, 5}, buckets)
	if got := histDeltaQuantile(same, same, 0.99); got != 0 {
		t.Errorf("no new events = %v, want 0", got)
	}
	if got := histDeltaQuantile(nil, nil, 0.99); got != 0 {
		t.Errorf("nil = %v", got)
	}
	// Counts that went backwards mean a reset: report nothing, not a garbage
	// unsigned wraparound.
	if got := histDeltaQuantile(rtHist([]uint64{9, 9}, buckets), rtHist([]uint64{1, 1}, buckets), 0.99); got != 0 {
		t.Errorf("reset = %v, want 0", got)
	}
	// No previous read: the whole histogram is the delta.
	if got := histDeltaQuantile(nil, rtHist([]uint64{10, 0}, buckets), 0.99); got != 0.001 {
		t.Errorf("first read = %v", got)
	}
}

func TestReadInternalsRealRuntime(t *testing.T) {
	now := time.Now()
	a, raw := readInternals(now, nil)
	if a.Goroutines == 0 || a.HeapObjectsBytes == 0 || a.HeapStacksBytes == 0 || a.GOGC == 0 {
		t.Fatalf("runtime figures missing: %+v", a)
	}
	if a.IntervalSeconds != 0 {
		t.Errorf("first read has no interval, got %v", a.IntervalSeconds)
	}
	runtime.GC()
	b, _ := readInternals(now.Add(2*time.Second), raw)
	if b.GCCycles <= a.GCCycles {
		t.Errorf("gc cycles %d -> %d, want an increase after runtime.GC", a.GCCycles, b.GCCycles)
	}
	if b.IntervalSeconds != 2 {
		t.Errorf("interval = %v, want 2", b.IntervalSeconds)
	}
	if b.GCCPUFraction < 0 || b.GCCPUFraction > 1 {
		t.Errorf("gc cpu fraction out of range: %v", b.GCCPUFraction)
	}
}

func TestInternalsCollectorCachesWithinTTL(t *testing.T) {
	c := &internalsCollector{}
	now := time.Now()
	a := c.Get(now)
	if b := c.Get(now.Add(internalsCacheTTL / 2)); b != a {
		t.Error("a read inside the TTL must reuse the cached value")
	}
	if b := c.Get(now.Add(internalsCacheTTL + time.Millisecond)); b == a {
		t.Error("a read past the TTL must rebuild")
	}
}

func TestGoroutineCollectorRealAndRateLimited(t *testing.T) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-stop }()
	}
	defer func() { close(stop); wg.Wait() }()

	c := &goroutineCollector{}
	now := time.Now()
	a := c.Get(now)
	if a.Total < 50 || len(a.Groups) == 0 {
		t.Fatalf("real profile = %+v", a)
	}
	found := false
	for _, g := range a.Groups {
		if strings.Contains(g.Func, "TestGoroutineCollectorRealAndRateLimited") && g.Count >= 50 {
			found = true
		}
	}
	if !found {
		t.Errorf("the 50 parked goroutines should group under the test func: %+v", a.Groups)
	}
	if b := c.Get(now.Add(goroutineGroupsTTL - time.Second)); b != a {
		t.Error("the profile must not be retaken inside the TTL")
	}
	if b := c.Get(now.Add(goroutineGroupsTTL + time.Second)); b == a {
		t.Error("the profile must be retaken past the TTL")
	}
}

// A viewer polling at 100ms must see the counters move on every read, while the
// windowed figures (quantiles, cpu share) only change when a window completes.
func TestInternalsCollectorCountersAreLiveWindowsAreNot(t *testing.T) {
	c := &internalsCollector{}
	now := time.Now()
	first := c.Get(now)
	if first.IntervalSeconds != 0 {
		t.Fatalf("no window has completed yet, got interval %v", first.IntervalSeconds)
	}
	sink := make([][]byte, 0, 64)
	var lastAlloc = first.AllocBytes
	for i := 1; i <= 9; i++ {
		sink = append(sink, make([]byte, 1<<16)) // real allocation between reads
		got := c.Get(now.Add(time.Duration(i) * internalsCacheTTL))
		if got.AllocBytes <= lastAlloc {
			t.Fatalf("read %d at +%dms: alloc counter %d did not advance from %d", i, i*100, got.AllocBytes, lastAlloc)
		}
		lastAlloc = got.AllocBytes
		if got.IntervalSeconds != 0 {
			t.Fatalf("window reported after only %dms", i*100)
		}
	}
	_ = sink
	// The window completes a second after it began, and is then held.
	done := c.Get(now.Add(internalsQuantileEvery))
	if done.IntervalSeconds != 1 {
		t.Fatalf("completed window = %vs, want 1", done.IntervalSeconds)
	}
	held := c.Get(now.Add(internalsQuantileEvery + 2*internalsCacheTTL))
	if held.IntervalSeconds != 1 {
		t.Fatalf("the finished window must be served until the next completes, got %v", held.IntervalSeconds)
	}
}
