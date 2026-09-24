package connect

import (
	"fmt"
	"sort"
	"strings"
)

// The pool dump prints returned/taken for every pool and tag, a wall of lines
// where nothing says which one is a problem. The summary reads the same
// numbers and says so.
//
// Return percentage is a poor signal on its own: buffers in flight when the
// dump runs count as taken but not yet returned, so a healthy tag can read
// 34% right after a restart and climb toward 100% by itself. What separates a
// leak from working memory is the trend of outstanding (taken - returned):
// held steady or plateauing is fine, growing steadily and not levelling off is
// not.
const (
	// poolTrendWindow is how many consecutive dumps (one a minute) a trend is
	// judged over.
	poolTrendWindow = 10

	// poolLeakFloor is the smallest growth in outstanding buffers over the
	// window that counts, and poolLeakMinGrowthPct the smallest relative
	// growth. Both must be met, so jitter on a big tag and a few buffers on a
	// tiny one are ignored while a slow steady leak is still caught.
	poolLeakFloor        = 64
	poolLeakMinGrowthPct = 5

	// poolLowReuse flags allocation churn: a tag that keeps creating new
	// buffers instead of reusing pooled ones. Only judged with real volume, so
	// a tag's first allocation (0% reuse over 1 take) is not a finding.
	poolLowReusePct   = 50
	poolLowReuseTakes = 10000
)

// poolTagStat is one row of the dump: the counters for a pool size and tag.
type poolTagStat struct {
	PoolSize int
	Tag      int
	Caller   string
	Taken    uint64
	Returned uint64
	Created  uint64
}

// outstanding is how many buffers are out right now. It never goes negative: a
// counter reset can leave returned ahead of taken for a moment.
func (s poolTagStat) outstanding() int64 {
	if s.Returned >= s.Taken {
		return 0
	}
	return int64(s.Taken - s.Returned)
}

func (s poolTagStat) key() string {
	return fmt.Sprintf("%d/%d", s.PoolSize, s.Tag)
}

// poolFinding names one culprit and why.
type poolFinding struct {
	PoolSize int
	Tag      int
	Caller   string
	Detail   string
}

func (f poolFinding) String() string {
	return fmt.Sprintf("pool[%d] tag=%d [%s] %s", f.PoolSize, f.Tag, f.Caller, f.Detail)
}

// poolSummary is the verdict for one dump.
type poolSummary struct {
	Tags        int
	Dumps       int  // dumps seen so far, capped at the window
	Warm        bool // enough dumps to judge a trend
	HeldBuffers int64
	HeldBytes   int64
	Growing     []poolFinding
	LowReuse    []poolFinding
}

// Healthy is true when nothing needs a look.
func (s poolSummary) Healthy() bool {
	return len(s.Growing) == 0 && len(s.LowReuse) == 0
}

func (s poolSummary) String() string {
	held := fmt.Sprintf("holding %.1f MiB in %d buffers", float64(s.HeldBytes)/(1<<20), s.HeldBuffers)
	if s.Healthy() {
		if !s.Warm {
			return fmt.Sprintf("pool summary: %d tags checked, %s, trend needs %d dumps (have %d)", s.Tags, held, poolTrendWindow, s.Dumps)
		}
		return fmt.Sprintf("pool summary: %d tags checked, all clear, %s", s.Tags, held)
	}
	var parts []string
	if n := len(s.Growing); n > 0 {
		parts = append(parts, fmt.Sprintf("%d possible leak: %s", n, joinFindings(s.Growing)))
	}
	if n := len(s.LowReuse); n > 0 {
		parts = append(parts, fmt.Sprintf("%d low reuse: %s", n, joinFindings(s.LowReuse)))
	}
	return fmt.Sprintf("pool summary: %d tags checked, %s, %s", s.Tags, strings.Join(parts, "; "), held)
}

func joinFindings(fs []poolFinding) string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.String()
	}
	return strings.Join(out, ", ")
}

// poolWatch remembers the last poolTrendWindow outstanding counts per pool and
// tag. It is used by one goroutine (the dump loop) and needs no lock.
type poolWatch struct {
	hist map[string][]int64
}

func newPoolWatch() *poolWatch {
	return &poolWatch{hist: map[string][]int64{}}
}

// observe records one dump and returns its summary.
func (w *poolWatch) observe(stats []poolTagStat) poolSummary {
	var sum poolSummary
	sum.Tags = len(stats)
	for _, st := range stats {
		out := st.outstanding()
		sum.HeldBuffers += out
		sum.HeldBytes += out * int64(st.PoolSize)

		h := append(w.hist[st.key()], out)
		if len(h) > poolTrendWindow {
			h = h[len(h)-poolTrendWindow:]
		}
		w.hist[st.key()] = h
		if len(h) > sum.Dumps {
			sum.Dumps = len(h)
		}

		if len(h) == poolTrendWindow && growingWithoutLevelling(h) {
			sum.Growing = append(sum.Growing, poolFinding{
				PoolSize: st.PoolSize, Tag: st.Tag, Caller: st.Caller,
				Detail: fmt.Sprintf("outstanding %d -> %d over %d dumps and still rising", h[0], h[len(h)-1], poolTrendWindow),
			})
		}
		if st.Taken >= poolLowReuseTakes {
			reuse := 100 * float64(st.Taken-min(st.Taken, st.Created)) / float64(st.Taken)
			if reuse < poolLowReusePct {
				sum.LowReuse = append(sum.LowReuse, poolFinding{
					PoolSize: st.PoolSize, Tag: st.Tag, Caller: st.Caller,
					Detail: fmt.Sprintf("%.0f%% reuse over %d takes", reuse, st.Taken),
				})
			}
		}
	}
	sum.Warm = sum.Dumps >= poolTrendWindow
	sortFindings(sum.Growing)
	sortFindings(sum.LowReuse)
	return sum
}

func sortFindings(fs []poolFinding) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].PoolSize != fs[j].PoolSize {
			return fs[i].PoolSize < fs[j].PoolSize
		}
		return fs[i].Tag < fs[j].Tag
	})
}

// growingWithoutLevelling reports whether outstanding grew steadily across the
// window and is still growing at close to its early pace. A plateau after a
// ramp (a working set filling after a restart) fails the last check, and
// jitter fails the monotonic check.
func growingWithoutLevelling(h []int64) bool {
	n := len(h)
	first, last := h[0], h[n-1]
	grew := last - first
	if grew < poolLeakFloor || grew*100 < first*poolLeakMinGrowthPct {
		return false
	}
	// Allow one dip: outstanding legitimately wobbles by a few buffers.
	nonDecreasing := 0
	for i := 1; i < n; i++ {
		if h[i] >= h[i-1] {
			nonDecreasing++
		}
	}
	if nonDecreasing < n-2 {
		return false
	}
	// Compare the pace of the first and last third. Still climbing at half the
	// early rate or better is a leak; flattening out is not.
	third := (n - 1) / 3
	early := h[third] - h[0]
	late := h[n-1] - h[n-1-third]
	return late > 0 && late*2 >= early
}

// poolCallerLabel joins call sites in a stable order. The label used to come
// straight out of a map, so one tag flipped between two spellings from dump to
// dump and could not be followed with grep.
func poolCallerLabel[V any](callers map[string]V) string {
	names := make([]string, 0, len(callers))
	for name := range callers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "/")
}
