package urnettools

import (
	"math"
	"time"
)

// The throughput graphs show one fixed, round time span at a time. `w` steps to
// the next span and `W` to the previous one, and every span fills the plot from
// edge to edge whatever the terminal width and the refresh speed. The list is
// one table so adding or retuning a span is a one line change.
var topGraphWindows = [4]time.Duration{
	10 * time.Second,
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

// topGraphWindowNames are the labels the titles and the footer show. They are
// pinned rather than formatted so a one minute window reads "1m", never "60s".
var topGraphWindowNames = [4]string{"10s", "1m", "5m", "15m"}

// topGraphWindowDefault is the level top starts on: one minute.
const topGraphWindowDefault = 1

// topGraphWindowIndex maps a saved name to its level. An unknown name is not a
// level, so a hand-edited or older settings file falls back to the default.
func topGraphWindowIndex(name string) (int, bool) {
	for i, n := range topGraphWindowNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

// graphWindow is the span the selected level draws.
func (m *topModel) graphWindow() time.Duration {
	if m.winLevel < 0 || m.winLevel >= len(topGraphWindows) {
		return topGraphWindows[topGraphWindowDefault]
	}
	return topGraphWindows[m.winLevel]
}

// stepGraphWindow moves the level by d, wrapping around the table.
func (m *topModel) stepGraphWindow(d int) {
	n := len(topGraphWindows)
	m.winLevel = ((m.winLevel+d)%n + n) % n
}

// remoteWindow is the span the Remote chart and the direction pages draw. They
// read the second by second ring, which holds topGraphCapacity seconds, so the
// longest level is capped there instead of drawing a window the data cannot
// fill.
func (m *topModel) remoteWindow() time.Duration {
	w := m.graphWindow()
	if c := time.Duration(topGraphCapacity) * time.Second; w > c {
		return c
	}
	return w
}

// topGraphRing keeps one reading per accepted traffic poll, with the time span
// the reading covers, so the graph can place readings on a time axis instead of
// counting columns. It is trimmed to the longest level, which bounds it at
// 15 minutes of readings whatever the refresh speed.
type topGraphRing struct {
	at              []int64
	spans           []time.Duration
	billable, total []float64
}

func (r *topGraphRing) reset() { *r = topGraphRing{} }

// push adds the reading that ended at at and covered span. A reading older than
// the newest one is ignored, which keeps the ring ordered when the provider
// clock steps back.
func (r *topGraphRing) push(at int64, span time.Duration, b, t float64) {
	if n := len(r.at); n > 0 && at <= r.at[n-1] {
		return
	}
	r.at = append(r.at, at)
	r.spans = append(r.spans, span)
	r.billable = append(r.billable, b)
	r.total = append(r.total, t)
	// Trim to the longest level, measured from the newest reading. One reading
	// is kept beside it so the covered span stays exact after trimming.
	if longest := topGraphWindows[len(topGraphWindows)-1]; longest > 0 {
		cut := at - int64(longest)
		n := 0
		for n < len(r.at)-1 && r.at[n] <= cut {
			n++
		}
		r.at, r.spans, r.billable, r.total = r.at[n:], r.spans[n:], r.billable[n:], r.total[n:]
	}
}

// cover is how much time the ring holds, and whether that reaches a full
// window. The oldest reading's own span counts, so a window is full as soon as
// the data reaches its left edge.
func (r *topGraphRing) cover(window time.Duration) (covered time.Duration, full bool) {
	if len(r.at) == 0 {
		return 0, false
	}
	start := r.at[0] - int64(r.spans[0])
	covered = time.Duration(r.at[len(r.at)-1] - start)
	return covered, covered >= window
}

// spreadSeries places readings on a time axis of n columns covering window,
// ending at the newest reading. A reading fills every column its span crosses,
// so a handful of readings become wide step bars and many readings keep the
// highest one in each column. Columns the data does not reach stay NaN, which
// draws as blank: a young window grows in from the right instead of stretching
// a few seconds across a quarter hour.
func spreadSeries(at []int64, spans []time.Duration, values []float64, newest int64, window time.Duration, n int) []float64 {
	if n <= 0 || len(at) == 0 || window <= 0 {
		return nil
	}
	n = min(n, 1<<20)
	readings := min(len(at), min(len(spans), len(values)))
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	start := newest - int64(window)
	cols := float64(n)
	width := float64(window)
	for i := readings - 1; i >= 0; i-- {
		v := values[i]
		if math.IsNaN(v) {
			continue
		}
		a := at[i] - int64(spans[i])
		b := at[i]
		if b < start || a > newest {
			break // everything older is outside the window too
		}
		c0 := int(float64(a-start) / width * cols)
		c1 := int(float64(b-start) / width * cols)
		c0 = max(c0, 0)
		c1 = min(c1, n-1)
		for c := c0; c <= c1; c++ {
			if math.IsNaN(out[c]) || v > out[c] {
				out[c] = v
			}
		}
	}
	return out
}
