package urnettools

import (
	"math"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// remoteSampleEase is how long the newest sample glides from the previous
// value to its new value, matching the client app's TRANSITION_MILLIS.
const remoteSampleEase = 500 * time.Millisecond

// topTimer is a single-shot timer owned by runTop. Production uses time.Timer;
// tests supply a fake so the loop's timer lifecycle is verified without sleeps.
type topTimer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type realTopTimer struct {
	t *time.Timer
}

func (r realTopTimer) Chan() <-chan time.Time { return r.t.C }

func (r realTopTimer) Stop() bool {
	if !r.t.Stop() {
		select {
		case <-r.t.C:
		default:
		}
		return false
	}
	return true
}

func newRealTopTimer(d time.Duration) topTimer {
	return realTopTimer{t: time.NewTimer(d)}
}

// nextDotBoundary is the wait from now until the next dot column boundary of
// remoteDotClock(now, window, dots), clamped to at least topMinFrame (20 fps).
// It returns 0 when the window or dot count cannot define a step.
func nextDotBoundary(now time.Time, window time.Duration, dots int) time.Duration {
	if dots < 2 || window <= 0 {
		return 0
	}
	step := int64(window) / int64(dots-1)
	if step < 1 {
		return topMinFrame
	}
	rem := now.UnixNano() % step
	if rem < 0 {
		rem += step
	}
	wait := time.Duration(step - rem)
	if wait < topMinFrame {
		wait = topMinFrame
	}
	return wait
}

// scalesEasing reports whether any scale in scales is mid-transition at now.
func scalesEasing(scales map[string]*remoteScale, now time.Time) bool {
	for _, s := range scales {
		if s == nil || !s.set || s.from == s.to {
			continue
		}
		if d := now.Sub(s.start); d >= 0 && d < remoteScaleEase {
			return true
		}
	}
	return false
}

// seriesHasActivity reports whether any finite positive sample in vals falls
// inside [now-window, now].
func seriesHasActivity(vals []float64, times []int64, newest time.Time, interval, window time.Duration, now time.Time) bool {
	if len(vals) == 0 || window <= 0 {
		return false
	}
	if interval <= 0 {
		interval = time.Second
	}
	windowStart := now.Add(-window)
	stored := len(times) == len(vals)
	for i := len(vals) - 1; i >= 0; i-- {
		at := newest.Add(-time.Duration(len(vals)-1-i) * interval)
		if stored {
			at = time.Unix(0, times[i])
		}
		if at.Before(windowStart) {
			break
		}
		if v := vals[i]; v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

// seriesNewestEasing reports whether the newest sample of vals is still
// gliding from its previous value at now.
func seriesNewestEasing(vals []float64, newest time.Time, now time.Time) bool {
	if len(vals) == 0 || newest.IsZero() {
		return false
	}
	elapsed := now.Sub(newest)
	if elapsed < 0 || elapsed >= remoteSampleEase {
		return false
	}
	cur, ok := lastFiniteValue(vals)
	if !ok {
		return false
	}
	prev := prevFiniteValue(vals)
	return cur != prev
}

// lastFiniteValue returns the last finite non-negative sample in vals.
func lastFiniteValue(vals []float64) (float64, bool) {
	for i := len(vals) - 1; i >= 0; i-- {
		if v := vals[i]; !math.IsNaN(v) && !math.IsInf(v, 0) {
			return max(v, 0), true
		}
	}
	return 0, false
}

// prevFiniteValue returns the finite non-negative sample immediately before
// the last finite sample in vals, or 0 when there is none.
func prevFiniteValue(vals []float64) float64 {
	seenLast := false
	for i := len(vals) - 1; i >= 0; i-- {
		if v := vals[i]; !math.IsNaN(v) && !math.IsInf(v, 0) {
			if !seenLast {
				seenLast = true
				continue
			}
			return max(v, 0)
		}
	}
	return 0
}

// splashCovering reports whether the startup splash is currently covering the
// screen, without starting or mutating the splash clock.
func (m *topModel) splashCovering(now time.Time) bool {
	s := &m.splash
	if !s.armed || s.done {
		return false
	}
	return s.until.IsZero() || now.Before(s.until)
}

// glideDelay reports when runTop should next redraw to advance a visible time
// graph, and false when no render tick should run (off-graph page, overlay or
// dialog covering the graph, disconnected, quitting, or static flat chart).
func (m *topModel) glideDelay(now time.Time) (time.Duration, bool) {
	if m.quit || m.conn != topConnected || m.snap == nil {
		return 0, false
	}
	if m.compact || m.overview || m.help || m.menu || m.about || m.panel != "" || m.proxyPopup || m.lingerFix.dialog {
		return 0, false
	}
	if m.splashCovering(now) {
		return 0, false
	}

	area := m.pageArea()
	switch m.selectedPage().id {
	case "remote":
		return m.remoteGlideDelay(now, max(area.W-2, 1))
	case "billable":
		return m.directionGlideDelay(now, max(area.W-2, 1), true)
	case "total":
		return m.directionGlideDelay(now, max(area.W-2, 1), false)
	case "overview":
		return m.overviewGlideDelay(now, area)
	default:
		return 0, false
	}
}

func (m *topModel) remoteGlideDelay(now time.Time, boxW int) (time.Duration, bool) {
	win := m.remoteWindow()
	dots := 2 * boxW
	easing := scalesEasing(m.remoteScales, now)
	active := false

	if upB, downB, upP, downP, newest, interval, ok := m.remoteDirData(); ok {
		times := m.ringTimes(len(upB))
		for _, s := range [][]float64{upB, downB, upP, downP} {
			if seriesNewestEasing(s, newest, now) {
				easing = true
			}
			if seriesHasActivity(s, times, newest, interval, win, now) {
				active = true
			}
		}
	} else if up, down, newest, interval, ok := m.remoteData(); ok {
		times := m.ringTimes(len(up))
		for _, s := range [][]float64{up, down} {
			if seriesNewestEasing(s, newest, now) {
				easing = true
			}
			if seriesHasActivity(s, times, newest, interval, win, now) {
				active = true
			}
		}
	}
	if !easing && !active {
		return 0, false
	}
	if easing {
		return topMinFrame, true
	}
	d := nextDotBoundary(now, win, dots)
	return d, d > 0
}

func (m *topModel) directionGlideDelay(now time.Time, boxW int, billable bool) (time.Duration, bool) {
	up, down, pUp, pDown, newest, interval, ok := m.directionData(billable)
	if !ok {
		return 0, false
	}
	win := m.remoteWindow()
	dots := 2 * boxW
	easing := scalesEasing(m.directionScales, now)
	active := false
	for _, s := range [][]float64{up, down, pUp, pDown} {
		if seriesNewestEasing(s, newest, now) {
			easing = true
		}
		if seriesHasActivity(s, nil, newest, interval, win, now) {
			active = true
		}
	}
	if !easing && !active {
		return 0, false
	}
	if easing {
		return topMinFrame, true
	}
	d := nextDotBoundary(now, win, dots)
	return d, d > 0
}

func (m *topModel) overviewGlideDelay(now time.Time, area tui.Rect) (time.Duration, bool) {
	leftW := max(area.W-m.sideCols(area.W)-2, 1)
	var best time.Duration
	have := false

	// If the embedded Remote panel is enabled, honor its glide cadence.
	if m.remote {
		if d, ok := m.remoteGlideDelay(now, leftW); ok {
			best, have = d, true
		}
	}

	// Check the overview throughput graphs.
	sr := m.series()
	win := m.graphWindow()
	perCell := 2
	if m.graph == tui.GraphBlock && !m.theme.ASCII {
		perCell = 1
	}
	dots := leftW * perCell
	active := false
	if sr.live && (sr.tailBillable > 0 || sr.tailTotal > 0) {
		active = true
	}
	if !active && sr.window && len(sr.at) > 0 {
		newest := time.Unix(0, sr.at[len(sr.at)-1])
		active = seriesHasActivity(sr.billable, sr.at, newest, time.Second, win, now) ||
			seriesHasActivity(sr.total, sr.at, newest, time.Second, win, now)
	}
	if !active && !sr.window && m.snap != nil {
		interval := time.Second
		if m.snap.Rate.HistoryIntervalSeconds > 0 {
			interval = time.Duration(m.snap.Rate.HistoryIntervalSeconds * float64(time.Second))
		}
		newest := m.snapNewest(interval)
		active = seriesHasActivity(sr.billable, nil, newest, interval, win, now) ||
			seriesHasActivity(sr.total, nil, newest, interval, win, now)
	}
	if active {
		if d := nextDotBoundary(now, win, dots); d > 0 && (!have || d < best) {
			best, have = d, true
		}
	}
	return best, have
}
