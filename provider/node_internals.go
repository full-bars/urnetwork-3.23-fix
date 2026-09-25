package main

import (
	"bytes"
	"math"
	"regexp"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runtime internals for `urnet-tools top`: what the Go runtime says about the
// provider process itself. Two commands serve it.
//
//   "internals"  a cheap read of runtime/metrics (18us at 95k goroutines),
//                cached for 100ms so any number of viewers cost one read and
//                a viewer polling at 100ms sees every figure move.
//   "goroutines" the goroutine profile grouped by function. This one stops the
//                world briefly (about 80ms of CPU and a ~9ms worst scheduler
//                stall measured at 95k goroutines), so it is cached and never
//                rebuilt more often than goroutineGroupsTTL.

const (
	// internalsCacheTTL is how long a read is reused. It matches urnet-tools
	// top's fastest poll, so counters (goroutines, allocations, GC cycles) are
	// live at that rate.
	internalsCacheTTL = 100 * time.Millisecond
	// internalsQuantileEvery is the window the pause and latency quantiles and
	// the GC cpu share are computed over. A histogram delta over 100ms holds a
	// handful of events, so a p99 of it is noise; over a second it is a figure.
	internalsQuantileEvery = time.Second
	// goroutineGroupsTTL bounds how often the goroutine profile is taken, no
	// matter how many viewers ask or how fast.
	goroutineGroupsTTL = 5 * time.Second
	// goroutineGroupsMax is how many groups the reply carries.
	goroutineGroupsMax = 12
)

// NodeInternals is the reply to "internals". Counters are cumulative and fresh
// to the cache TTL; the pause and latency quantiles and the GC cpu share cover
// the last completed window of at least internalsQuantileEvery, so they describe
// now, not the process lifetime.
type NodeInternals struct {
	AtUnixNano int64 `json:"at_unix_nano"`

	Goroutines uint64 `json:"goroutines"`

	HeapObjectsBytes uint64 `json:"heap_objects_bytes"`
	HeapStacksBytes  uint64 `json:"heap_stacks_bytes"`
	HeapGoalBytes    uint64 `json:"heap_goal_bytes"`
	GOGC             int64  `json:"gogc"`

	GCCycles   uint64 `json:"gc_cycles"`
	AllocBytes uint64 `json:"alloc_bytes"` // cumulative bytes allocated

	// Over the last interval.
	IntervalSeconds float64 `json:"interval_seconds"`
	GCPauseP99Ms    float64 `json:"gc_pause_p99_ms"`
	SchedLatP99Ms   float64 `json:"sched_latency_p99_ms"`
	GCCPUFraction   float64 `json:"gc_cpu_fraction"` // GC cpu / total cpu
}

// GoroutineGroup is the goroutines parked in one place: Func is the first frame
// outside the runtime, Count how many are there.
type GoroutineGroup struct {
	Func  string `json:"func"`
	Count int    `json:"count"`
}

// GoroutineGroups is the reply to "goroutines".
type GoroutineGroups struct {
	AtUnixNano int64            `json:"at_unix_nano"`
	Total      int              `json:"total"`
	Groups     []GoroutineGroup `json:"groups"`
}

var internalsMetricNames = []string{
	"/sched/goroutines:goroutines",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/gc/heap/goal:bytes",
	"/gc/gogc:percent",
	"/gc/cycles/total:gc-cycles",
	"/gc/heap/allocs:bytes",
	"/gc/pauses:seconds",
	"/sched/latencies:seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/total:cpu-seconds",
}

type internalsCollector struct {
	mu       sync.Mutex
	cached   *NodeInternals
	cachedAt time.Time
	// base is the read the current quantile window started at, and window the
	// figures of the last completed one, served until the next completes.
	base   *internalsRaw
	window NodeInternals
}

// internalsRaw is what one read leaves behind for the next one to diff against.
type internalsRaw struct {
	at       time.Time
	pauses   *metrics.Float64Histogram
	sched    *metrics.Float64Histogram
	gcCPU    float64
	totalCPU float64
}

var nodeInternals = &internalsCollector{}

// Get returns the internals, rebuilt at most once per internalsCacheTTL.
func (c *internalsCollector) Get(now time.Time) *NodeInternals {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && now.Sub(c.cachedAt) < internalsCacheTTL {
		return c.cached
	}
	out, raw := readInternals(now, nil)
	switch {
	case c.base == nil:
		c.base = raw
	case now.Sub(c.base.at) >= internalsQuantileEvery:
		var w NodeInternals
		applyInternalsDeltas(&w, c.base, raw)
		c.window, c.base = w, raw
	}
	out.IntervalSeconds = c.window.IntervalSeconds
	out.GCPauseP99Ms = c.window.GCPauseP99Ms
	out.SchedLatP99Ms = c.window.SchedLatP99Ms
	out.GCCPUFraction = c.window.GCCPUFraction
	c.cached, c.cachedAt = out, now
	return c.cached
}

func readInternals(now time.Time, prev *internalsRaw) (*NodeInternals, *internalsRaw) {
	samples := make([]metrics.Sample, len(internalsMetricNames))
	for i, n := range internalsMetricNames {
		samples[i].Name = n
	}
	metrics.Read(samples)
	u := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	f := func(i int) float64 {
		if samples[i].Value.Kind() == metrics.KindFloat64 {
			return samples[i].Value.Float64()
		}
		return 0
	}
	hist := func(i int) *metrics.Float64Histogram {
		if samples[i].Value.Kind() == metrics.KindFloat64Histogram {
			return samples[i].Value.Float64Histogram()
		}
		return nil
	}
	out := &NodeInternals{
		AtUnixNano:       now.UnixNano(),
		Goroutines:       u(0),
		HeapObjectsBytes: u(1),
		HeapStacksBytes:  u(2),
		HeapGoalBytes:    u(3),
		GOGC:             int64(u(4)),
		GCCycles:         u(5),
		AllocBytes:       u(6),
	}
	raw := &internalsRaw{at: now, pauses: hist(7), sched: hist(8), gcCPU: f(9), totalCPU: f(10)}
	applyInternalsDeltas(out, prev, raw)
	return out, raw
}

// applyInternalsDeltas fills the over-a-window figures of out from two reads.
// With no previous read they stay zero.
func applyInternalsDeltas(out *NodeInternals, prev, cur *internalsRaw) {
	if prev == nil || !cur.at.After(prev.at) {
		return
	}
	out.IntervalSeconds = cur.at.Sub(prev.at).Seconds()
	out.GCPauseP99Ms = histDeltaQuantile(prev.pauses, cur.pauses, 0.99) * 1000
	out.SchedLatP99Ms = histDeltaQuantile(prev.sched, cur.sched, 0.99) * 1000
	if dt := cur.totalCPU - prev.totalCPU; dt > 0 {
		out.GCCPUFraction = math.Min(math.Max((cur.gcCPU-prev.gcCPU)/dt, 0), 1)
	}
}

// histDeltaQuantile is the q-quantile of what was recorded between two reads of
// a cumulative runtime histogram, as the upper edge of the bucket it falls in.
// Zero when nothing was recorded (or the buckets changed shape).
func histDeltaQuantile(prev, cur *metrics.Float64Histogram, q float64) float64 {
	if cur == nil || len(cur.Counts) == 0 {
		return 0
	}
	delta := make([]uint64, len(cur.Counts))
	var total uint64
	for i, c := range cur.Counts {
		d := c
		if prev != nil && len(prev.Counts) == len(cur.Counts) {
			if prev.Counts[i] > c {
				return 0 // not cumulative: something reset, say nothing
			}
			d = c - prev.Counts[i]
		}
		delta[i] = d
		total += d
	}
	if total == 0 {
		return 0
	}
	want := uint64(math.Ceil(q * float64(total)))
	var run uint64
	for i, d := range delta {
		run += d
		if run >= want {
			// Buckets[i+1] is the upper edge of bucket i; the last may be +Inf,
			// in which case the lower edge is the honest bound.
			if hi := cur.Buckets[i+1]; !math.IsInf(hi, 1) {
				return hi
			}
			return cur.Buckets[i]
		}
	}
	return 0
}

// goroutineHeader matches "12 @ 0x... 0x..." starting each group of the
// debug=1 goroutine profile.
var goroutineHeader = regexp.MustCompile(`^(\d+) @`)

// parseGoroutineProfile turns the debug=1 text of the goroutine profile into
// groups keyed by the first frame outside the runtime, so a hundred thousand
// goroutines parked in gopark read as what they are waiting on. Groups are
// sorted by count, largest first, and cut to limit.
func parseGoroutineProfile(text string, limit int) GoroutineGroups {
	counts := map[string]int{}
	total := 0
	cur, want := 0, false
	flush := func(fn string) {
		if cur > 0 && want {
			counts[fn] += cur
			total += cur
			want = false
		}
	}
	var first string
	for _, line := range strings.Split(text, "\n") {
		if m := goroutineHeader.FindStringSubmatch(line); m != nil {
			if want { // previous group had no usable frame
				flush(first)
			}
			cur, _ = strconv.Atoi(m[1])
			want, first = true, ""
			continue
		}
		if strings.HasPrefix(line, "# labels") {
			// pprof debug=1 emits "# labels: {\"k\":\"v\"}" between the
			// header and the frames; it is profile metadata, not a frame,
			// and must not start (and end) a group.
			continue
		}
		if !want || !strings.HasPrefix(line, "#") {
			continue
		}
		fn := frameFunc(line)
		if first == "" {
			first = fn // fallback: the very top frame
		}
		if !runtimeFrame(fn) {
			flush(fn)
		}
	}
	if want {
		flush(first)
	}
	groups := make([]GoroutineGroup, 0, len(counts))
	for fn, n := range counts {
		groups = append(groups, GoroutineGroup{Func: fn, Count: n})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Func < groups[j].Func
	})
	if len(groups) > limit {
		groups = groups[:limit]
	}
	return GoroutineGroups{Total: total, Groups: groups}
}

// frameFunc extracts "pkg.Func" from a profile frame line
// "#\t0x4a1b2c\tpkg.Func+0x1c\t/path/file.go:12".
func frameFunc(line string) string {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return strings.TrimSpace(strings.TrimPrefix(line, "#"))
	}
	fn := fields[2]
	if i := strings.LastIndex(fn, "+0x"); i > 0 {
		fn = fn[:i]
	}
	return fn
}

// runtimeFrame is a frame that says nothing about what a goroutine is for.
func runtimeFrame(fn string) bool {
	for _, p := range []string{"runtime.", "sync.", "internal/", "syscall.", "os.", "io.", "bufio.", "crypto/tls.", "net.(*"} {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	return false
}

type goroutineCollector struct {
	mu       sync.Mutex
	cached   *GoroutineGroups
	cachedAt time.Time
}

var nodeGoroutines = &goroutineCollector{}

// Get returns the grouped goroutine profile, taken at most once per
// goroutineGroupsTTL. Concurrent callers wait for the one profile in flight.
func (c *goroutineCollector) Get(now time.Time) *GoroutineGroups {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && now.Sub(c.cachedAt) < goroutineGroupsTTL {
		return c.cached
	}
	var buf bytes.Buffer
	if p := pprof.Lookup("goroutine"); p != nil {
		_ = p.WriteTo(&buf, 1)
	}
	g := parseGoroutineProfile(buf.String(), goroutineGroupsMax)
	g.AtUnixNano = now.UnixNano()
	c.cached, c.cachedAt = &g, now
	return c.cached
}
