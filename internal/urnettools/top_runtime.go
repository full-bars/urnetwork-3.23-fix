package urnettools

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The parts of `urnet-tools top` that sit beside the throughput picture: the
// zoom window, the steady graph axis and the runtime (Internals) panel. All of it
// is fed by the run loop and drawn from the model alone, like the rest.

const (
	// topZoomWindow is how much time the zoom graph shows, at whatever poll rate
	// is set: a 100ms poll gives 150 columns, a 1s poll 15.
	topZoomWindow = 15 * time.Second
	// topZoomSpan is the span each zoom column's rate is taken over. Two or
	// three polls at 100ms: motion on every poll without counter-granularity
	// noise.
	topZoomSpan = 250 * time.Millisecond
	// topZoomMinCols keeps the ring useful at slow poll rates.
	topZoomMinCols = 8

	// topScaleHeadroom is how far above the peak a raised axis is put, so a burst
	// a little above the last one does not rescale the chart again.
	topScaleHeadroom = 1.25
	// topScaleHold is how long the peak must stay well under the axis before it
	// comes back down; topScaleLowFrac is what "well under" means.
	topScaleHold    = 30 * time.Second
	topScaleLowFrac = 0.6

	// topRuntimeHistory is how many goroutine counts the trend keeps, one per
	// second however fast the poll is.
	topRuntimeHistory = 120
	// topRateSpan is the window a counter's rate is taken over. At a 100ms poll,
	// two adjacent readings would show a GC counter as 0, 0, 10/s, 0: a rate
	// means events over a stretch long enough to have some.
	topRateSpan = time.Second
	// topRecentKeep bounds the readings held for those rates.
	topRecentKeep = 2 * topRateSpan
	// topGroupsEvery is how often the goroutine profile is asked for while shown.
	// The provider rate-limits itself to the same period; asking faster gains
	// nothing.
	topGroupsEvery = 5 * time.Second

	// Internals panel geometry: frame plus eight rows, and the goroutine list
	// fits the same box.
	topInternalsRows = 10
)

// topScale holds one graph's axis top steady. The top rises at once when the
// data outgrows it (to a value with headroom), and comes back down only after the peak has
// stayed well under it for topScaleHold, so a burst scrolling off the left edge
// does not zoom the whole chart in the moment it goes.
type topScale struct {
	top      float64
	lowSince time.Time
}

// update folds in the current peak of the series and returns the axis top to
// use, zero when there is nothing to scale to.
func (s *topScale) update(peak float64, now time.Time, binary bool) float64 {
	want := tui.NiceCeil(peak*topScaleHeadroom, binary)
	switch {
	case peak > s.top:
		// The data no longer fits: raise at once, with headroom, so the next
		// burst a little higher does not need another rescale.
		s.top, s.lowSince = want, time.Time{}
	case want <= s.top*topScaleLowFrac:
		if s.lowSince.IsZero() {
			s.lowSince = now
		} else if now.Sub(s.lowSince) >= topScaleHold {
			s.top, s.lowSince = want, time.Time{}
		}
	default:
		s.lowSince = time.Time{}
	}
	return s.top
}

func (s *topScale) reset() { *s = topScale{} }

func seriesPeak(v []float64) float64 {
	p := 0.0
	for _, x := range v {
		if x > p {
			p = x
		}
	}
	return p
}

// topZoom is the client-side ring behind the zoom graph: one rate per traffic
// poll, newest last. seq counts every column ever pushed and anchors the graph,
// so a column keeps its place as older ones fall off the left.
type topZoom struct {
	billable, total []float64
	seq             int64
}

func (z *topZoom) reset() { *z = topZoom{} }

func (z *topZoom) push(b, t float64, capacity int) {
	z.billable = append(z.billable, b)
	z.total = append(z.total, t)
	z.seq++
	if n := len(z.billable) - capacity; n > 0 {
		z.billable, z.total = z.billable[n:], z.total[n:]
	}
}

// zoomCapacity is how many columns the ring holds at the current poll rate.
func (m *topModel) zoomCapacity() int {
	return max(int(topZoomWindow/max(m.interval, topMinInterval)), topZoomMinCols)
}

// topRuntime is what the Internals panel shows and how it is fed.
type topRuntime struct {
	ok      bool // false once the provider says it does not know the command
	busy    bool
	last    time.Time
	cur     *NodeInternals
	recent  []*NodeInternals // readings of the last topRecentKeep, for rates
	gor     []float64        // goroutine count per second, oldest first
	gorAt   int64            // provider clock of the newest gor point
	groups  *GoroutineGroups
	showing bool // the goroutine list replaces the runtime rows
	gOK     bool
	gBusy   bool
	gLast   time.Time
}

func (r *topRuntime) reset() { *r = topRuntime{ok: true, gOK: true} }

// wantInternals is wantTraffic for the runtime read: every poll interval. The
// provider caches it for 100ms, so the counters move at the poll rate.
func (m *topModel) wantInternals(now time.Time) (gen int, p Provider, ok bool) {
	r := &m.rt
	if r.busy || !r.ok || m.conn != topConnected || len(m.providers) == 0 {
		return 0, Provider{}, false
	}
	if !r.last.IsZero() && now.Sub(r.last) < m.interval {
		return 0, Provider{}, false
	}
	r.busy, r.last = true, now
	return m.gen, m.provider(), true
}

// applyInternals folds one runtime reading in. A missed reading only skips a
// sample; a provider that does not know the command drops the panel.
func (m *topModel) applyInternals(gen int, in *NodeInternals, err error) {
	if gen != m.gen {
		return
	}
	r := &m.rt
	r.busy = false
	if m.conn != topConnected {
		return
	}
	switch {
	case errors.Is(err, errRuntimeViewUnsupported):
		r.ok, r.gOK, r.showing = false, false, false
		r.cur, r.recent, r.groups, r.gor = nil, nil, nil, nil
	case err != nil || in == nil:
	default:
		r.cur = in
		r.recent = append(r.recent, in)
		// Drop readings older than the rate window needs, and never fewer than
		// two so a slow poll still has a span.
		for len(r.recent) > 2 && time.Duration(in.AtUnixNano-r.recent[1].AtUnixNano) >= topRecentKeep {
			r.recent = r.recent[1:]
		}
		// One trend point a second: at a 100ms poll the trend would otherwise
		// cover 12 seconds, not two minutes.
		if len(r.gor) == 0 || time.Duration(in.AtUnixNano-r.gorAt) >= time.Second {
			r.gor = append(r.gor, float64(in.Goroutines))
			r.gorAt = in.AtUnixNano
			if n := len(r.gor) - topRuntimeHistory; n > 0 {
				r.gor = r.gor[n:]
			}
		}
	}
}

// wantGroups asks for the goroutine profile, only while the list is shown.
func (m *topModel) wantGroups(now time.Time) (gen int, p Provider, ok bool) {
	r := &m.rt
	if !r.showing || r.gBusy || !r.gOK || m.conn != topConnected || len(m.providers) == 0 {
		return 0, Provider{}, false
	}
	if !r.gLast.IsZero() && now.Sub(r.gLast) < topGroupsEvery {
		return 0, Provider{}, false
	}
	r.gBusy, r.gLast = true, now
	return m.gen, m.provider(), true
}

func (m *topModel) applyGroups(gen int, g *GoroutineGroups, err error) {
	if gen != m.gen {
		return
	}
	r := &m.rt
	r.gBusy = false
	if m.conn != topConnected {
		return
	}
	switch {
	case errors.Is(err, errRuntimeViewUnsupported):
		r.gOK, r.showing = false, false
	case err != nil || g == nil:
	default:
		r.groups = g
	}
}

// hasInternals reports whether there is a runtime reading to draw.
func (m *topModel) hasInternals() bool { return m.rt.ok && m.rt.cur != nil }

// internalsRate is a counter's rate over the newest stretch of at least
// topRateSpan, per second; a younger series uses what it has once that is at
// least a quarter second. ok is false with no usable span, or when the counter
// went backwards (the provider restarted between readings).
func (r *topRuntime) internalsRate(get func(*NodeInternals) uint64) (float64, bool) {
	if r.cur == nil || len(r.recent) < 2 {
		return 0, false
	}
	ref := r.recent[0]
	for i := len(r.recent) - 2; i >= 0; i-- {
		if time.Duration(r.cur.AtUnixNano-r.recent[i].AtUnixNano) >= topRateSpan {
			ref = r.recent[i]
			break
		}
	}
	span := time.Duration(r.cur.AtUnixNano - ref.AtUnixNano)
	a, b := get(ref), get(r.cur)
	if span < topZoomSpan || b < a {
		return 0, false
	}
	return float64(b-a) / span.Seconds(), true
}

// shortFunc trims a profile function name to what fits a narrow column: the
// import path goes, "connect.(*Client).run" stays.
func shortFunc(fn string) string {
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		fn = fn[i+1:]
	}
	fn = strings.TrimPrefix(fn, "connect.")
	// "(*Client).run" is "Client.run": four cells saved, which in a 28 wide
	// panel is the difference between telling two long names apart or not.
	return ptrRecv.ReplaceAllString(fn, "$1")
}

var ptrRecv = regexp.MustCompile(`\(\*([^)]+)\)`)
