package urnettools

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The parts of `urnet-tools top` that sit beside the throughput picture: the
// zoom window, the steady graph axis and the runtime (Internals) panel. All of it
// is fed by the run loop and drawn from the model alone, like the rest.

const (
	// topZoomWindow is the duration shown by the zoom graph.
	topZoomWindow = 15 * time.Second
	// topZoomSpan is the span each zoom column's rate is taken over. Two or
	// three polls at 100ms: motion on every poll without counter-granularity
	// noise.
	topZoomSpan = 250 * time.Millisecond

	// topScaleHeadroom is how far above the peak a raised axis is put, so a burst
	// a little above the last one does not rescale the chart again.
	topScaleHeadroom = 1.25
	// topScaleHold is how long the peak must stay well under the axis before it
	// comes back down; topScaleLowFrac is what "well under" means.
	topScaleHold    = 30 * time.Second
	topScaleLowFrac = 0.6

	// topRuntimeHistory bounds the goroutine trend.
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
	// fits the same box. The transport split draws inside whatever height is
	// left, so it never changes whether the panel appears at all.
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

// topTrafficHistory keeps sampled rates and the time represented by each column.
// Slow polling uses one-second slots and leaves missing seconds unknown.
type topTrafficHistory struct {
	billable, total []float64
	second          int64
	spans           []time.Duration
	seq             int64
}

func (h *topTrafficHistory) push(at int64, b, t float64) {
	h.pushSlot(at, b, t)
}

func (h *topTrafficHistory) pushSlot(at int64, b, t float64) {
	second := at / int64(time.Second)
	if len(h.billable) > 0 && second < h.second {
		return
	}
	if len(h.billable) == 0 {
		h.second = second
		h.billable, h.total = []float64{b}, []float64{t}
		h.spans = []time.Duration{time.Second}
		h.seq++
		return
	}
	gap := second - h.second
	if gap >= topGraphCapacity {
		h.billable, h.total, h.spans = nil, nil, nil
		gap = topGraphCapacity
	}
	for i := int64(0); i < gap; i++ {
		h.billable = append(h.billable, math.NaN())
		h.total = append(h.total, math.NaN())
		h.spans = append(h.spans, time.Second)
		h.seq++
	}
	if len(h.billable) > topGraphCapacity {
		h.billable = h.billable[len(h.billable)-topGraphCapacity:]
		h.total = h.total[len(h.total)-topGraphCapacity:]
		h.spans = h.spans[len(h.spans)-topGraphCapacity:]
	}
	h.second = second
	h.billable[len(h.billable)-1], h.total[len(h.total)-1] = b, t
}

// pushColumn retains a reading with its actual elapsed span.
func (h *topTrafficHistory) pushColumn(at int64, span time.Duration, b, t float64) {
	const keep = 8192
	h.billable = append(h.billable, b)
	h.total = append(h.total, t)
	h.spans = append(h.spans, span)
	h.second = at / int64(time.Second)
	h.seq++
	if len(h.billable) > keep {
		h.billable = h.billable[len(h.billable)-keep:]
		h.total = h.total[len(h.total)-keep:]
		h.spans = h.spans[len(h.spans)-keep:]
	}
}

// topZoom stores rates with the provider time of each accepted poll.
type topZoom struct {
	billable, total []float64
	at              []int64
	spans           []time.Duration
	seq             int64
}

func (z *topZoom) reset() { *z = topZoom{} }

func (z *topZoom) push(at int64, b, t float64) {
	z.billable = append(z.billable, b)
	z.total = append(z.total, t)
	span := time.Second
	if len(z.at) > 0 {
		span = time.Duration(at - z.at[len(z.at)-1])
	}
	z.at = append(z.at, at)
	z.spans = append(z.spans, span)
	z.seq++
	cut := at - int64(topZoomWindow)
	n := 0
	for n < len(z.at) && z.at[n] <= cut {
		n++
	}
	z.at, z.billable, z.total, z.spans = z.at[n:], z.billable[n:], z.total[n:], z.spans[n:]
}

// grid places readings on a fixed time axis. Empty slots remain unknown.
func (z *topZoom) grid() (billable, total []float64, anchor int64) {
	if len(z.at) == 0 {
		return nil, nil, 0
	}
	const capacity = int(topZoomWindow / topMinInterval)
	anchor = z.at[len(z.at)-1] / int64(topMinInterval)
	billable, total = make([]float64, capacity), make([]float64, capacity)
	for i := range billable {
		billable[i], total[i] = math.NaN(), math.NaN()
	}
	for i, at := range z.at {
		idx := int(at/int64(topMinInterval) - anchor + int64(capacity) - 1)
		if idx >= 0 && idx < capacity {
			billable[idx], total[idx] = z.billable[i], z.total[i]
		}
	}
	return billable, total, anchor
}

func (m *topModel) zoomCapacity() int {
	return int(topZoomWindow / topMinInterval)
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
	if m.isSlow() {
		return 0, Provider{}, false // optional: the first thing dropped when the provider struggles
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
		// Keep each fast reading, or one point per second with slow polling.
		if len(r.gor) == 0 || (in.AtUnixNano > r.gorAt && (m.interval <= time.Second || time.Duration(in.AtUnixNano-r.gorAt) >= time.Second)) {
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
	if m.isSlow() {
		return 0, Provider{}, false // the goroutine profile is the costliest thing top asks for
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

// hasTransport reports whether the provider has sent the per-mode transport
// split. A box that has never attempted H3 sends none, and the panel then draws
// exactly the rows it drew before.
func (m *topModel) hasTransport() bool {
	return m.rt.cur != nil && m.rt.cur.Transport != nil
}

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
