package urnettools

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The Remote panel draws the mirrored transfer chart of the URnetwork client's
// statistics screen. One direction is drawn above a center line and the other
// below it, each as a thin curve through the samples, with the peak value
// labelled where it happened. The newest data is on the right
// and slides left as the clock moves.
//
// Everything in this file is a pure function of the chart input and the buffer
// it draws into: no terminal, no clock of its own, no globals. The caller owns
// the easing state of each scale and advances the clock, so a frame can be
// golden-tested at a fixed time.
//
// A half takes a list of series, so an upload curve, a download curve and a
// packet curve slot in beside the byte curve without another renderer. Series
// that share a Group share one scale, which is what keeps two curves of the
// same quantity comparable.

const (
	// remoteScaleEase is how long a scale takes to reach a new window peak.
	remoteScaleEase = 500 * time.Millisecond
	// remoteScaleMinWindow is the shortest stretch of history a scale looks
	// at, however short the zoom.
	remoteScaleMinWindow = time.Minute
	// remoteScaleShrink is how far the window peak must fall below the scale
	// before the scale follows it down.
	remoteScaleShrink = 0.15
	// remoteAverageBuckets is how many of the newest samples the corner
	// average covers.
	remoteAverageBuckets = 5
	// remoteDefaultWindow is how much time the plot spans by default.
	remoteDefaultWindow = 60 * time.Second
	// remoteMinWidth is the narrowest plot that still reads as a chart.
	remoteMinWidth = 8
	// remoteByteFloor and remotePacketFloor are the smallest heights a byte
	// and a packet scale draw against, the client app's values, so a few
	// bytes or packets of background noise stay near the axis.
	remoteByteFloor   = 1024
	remotePacketFloor = 8
)

// Braille dot bits for one cell, bottom row first, the same packing the
// throughput graphs use.
var (
	remoteBrailleLeft  = [4]rune{0x40, 0x04, 0x02, 0x01}
	remoteBrailleRight = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// remoteSeries is one curve: evenly spaced samples, oldest first, the newest at
// the chart's Newest time. Group names the scale it is drawn against. Glyph is
// the character the ASCII fallback draws; zero picks one from the series order.
// Packets marks a packet rate series, formatted in pkt/s rather than bytes per
// second in the corner figures.
type remoteSeries struct {
	Label   string
	Style   tui.Style
	Group   string
	Glyph   rune
	Packets bool
	Values  []float64
}

// remoteChart is one frame of the mirrored chart. Now is the clock the plot
// scrolls with, Newest the time of each series' newest sample. Scales holds the
// easing state per group, created on first use; the renderer folds the window
// peak into it, so the same map is passed again next frame.
type remoteChart struct {
	Up, Down []remoteSeries
	Now      time.Time
	Newest   time.Time
	// Times dates each sample of the series, when the source stored a date per
	// sample. It then replaces the even spacing Newest and Interval imply.
	Times    []int64
	Interval time.Duration
	Window   time.Duration
	Scales   map[string]*remoteScale
	// Shared draws both halves against one byte scale and one packet scale,
	// as the client app does, instead of each series group on its own.
	Shared bool
	ASCII  bool
	// dots is the plot width in braille dot columns, set by drawRemoteChart.
	dots int
	// plotNow is Now moved back to a whole dot column of the window, set by
	// drawRemoteChart. Every sample then lands a whole number of columns from
	// where it was a frame ago, so the curve scrolls without being sampled at
	// a new phase and reshaping. It is zero when the chart is built by hand.
	plotNow time.Time
	Dim     tui.Style // center line, legends and the peak labels
	Text    tui.Style // the direction arrows when a direction is active
}

// remoteScale eases the height the plot scales to. A new window peak starts a
// short ease from wherever the scale is now, so the curve settles into a new
// peak instead of jumping.
type remoteScale struct {
	from, to float64
	start    time.Time
	set      bool
}

// target folds in the peak of the window and starts an ease when it changed.
func (s *remoteScale) target(peak float64, now time.Time) {
	if peak <= 0 || math.IsNaN(peak) || math.IsInf(peak, 0) {
		peak = 1
	}
	if !s.set {
		s.from, s.to, s.start, s.set = peak, peak, now, true
		return
	}
	// A higher peak is taken at once, so the curve never runs off the plot. A
	// lower one is taken only when it fell by more than remoteScaleShrink.
	// The drawn peak moves by a fraction of a percent on most frames, and a
	// new ease for each of those nudges every dot of the plot up or down a
	// little, which reads as the whole curve shimmering.
	if peak == s.to || (peak < s.to && peak >= s.to*(1-remoteScaleShrink)) {
		return
	}
	s.from, s.to, s.start = s.valueAt(now), peak, now
}

// remoteEaseProgress is the ease-out cubic fraction in [0, 1] for elapsed time
// over dur, matching the client app's TRANSITION_MILLIS easing.
func remoteEaseProgress(elapsed, dur time.Duration) float64 {
	if dur <= 0 || elapsed >= dur {
		return 1
	}
	if elapsed <= 0 {
		return 0
	}
	p := float64(elapsed) / float64(dur)
	q := 1 - p
	return 1 - q*q*q
}

// valueAt is the eased scale at now.
func (s *remoteScale) valueAt(now time.Time) float64 {
	if !s.set {
		return 1
	}
	eased := remoteEaseProgress(now.Sub(s.start), remoteScaleEase)
	return s.from + (s.to-s.from)*eased
}

// remoteLayout places the parts of one frame. On a frame of 9 rows or more it
// puts the up figures on the first row and the down figures on the second, both
// right-aligned, then the up peak label, the up plot, the center row, the down
// plot and the down peak label. Both halves fill the remaining panel height
// equally, with (h-5)/2 rows each; when h-5 is odd, the spare row stays at the
// bottom. A shorter frame keeps the up figures on top and the down figures on
// the last row, with no peak labels.
type remoteChartLayout struct {
	header, footer, center int
	upY, upRows            int
	downY, downRows        int
	upPeak, downPeak       int // -1 when the half has no room for a label
}

func remoteLayout(w, h int) remoteChartLayout {
	l := remoteChartLayout{header: 0, footer: h - 1, upPeak: -1, downPeak: -1}
	if h < 3 {
		l.footer = -1
		return l
	}
	if h >= 9 {
		half := (h - 5) / 2
		l.footer = 1
		l.upPeak, l.upY, l.upRows = 2, 3, half
		l.center = l.upY + half
		l.downY, l.downRows = l.center+1, half
		l.downPeak = l.downY + half
		return l
	}
	plotRows := h - 3
	l.upRows = (plotRows + 1) / 2
	l.downRows = plotRows - l.upRows
	l.center = 1 + l.upRows
	l.upY = 1
	l.downY = l.center + 1
	return l
}

// remoteSample is one sample with the time it was taken, in Unix nanoseconds.
type remoteSample struct {
	at int64
	v  float64
}

// remotePoint is one place on a curve in dot space: x is a dot column and v the
// value at that column.
type remotePoint struct {
	x, v float64
}

// drawRemoteChart draws one frame of the chart into b.
func drawRemoteChart(b *tui.Buffer, c remoteChart) {
	w, h := b.Width(), b.Height()
	if w <= 0 || h <= 0 {
		return
	}
	if h < 3 || w < remoteMinWidth {
		drawRemoteSummary(b, c)
		return
	}
	if c.Window <= 0 {
		c.Window = remoteDefaultWindow
	}
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	c.dots = 2 * w
	c.plotNow = remoteDotClock(c.Now, c.Window, c.dots)
	scales := remoteScales(c)
	l := remoteLayout(w, h)

	if len(c.Up) == 0 && len(c.Down) == 0 {
		b.Put(tui.Truncate("remote: waiting for traffic", w, c.ASCII), 0, l.header, c.Dim)
		return
	}
	c.drawAverages(b, l.header, c.Up, '▲')
	if c.ASCII {
		for x := 0; x < w; x++ {
			b.Set(x, l.center, tui.Cell{Rune: '-', Style: c.Dim})
		}
		c.drawHalfASCIIAll(b, l, c.Up, true, scales)
		c.drawHalfASCIIAll(b, l, c.Down, false, scales)
	} else {
		c.drawPlot(b, l, scales)
	}
	c.drawPeak(b, c.Up, true, l.upPeak)
	c.drawPeak(b, c.Down, false, l.downPeak)
	if l.footer >= 0 {
		if len(c.Down) == 0 {
			b.Put(tui.Truncate("total not reported", w, c.ASCII), 0, l.footer, c.Dim)
			return
		}
		c.drawAverages(b, l.footer, c.Down, '▼')
	}
}

// drawRemoteSummary is the whole panel for a height that cannot hold the
// mirror: one line with the two current averages.
func drawRemoteSummary(b *tui.Buffer, c remoteChart) {
	line := "remote: waiting for traffic"
	up, down, ok := remoteNewest(c.Up), remoteNewest(c.Down), false
	if len(c.Up) > 0 || len(c.Down) > 0 {
		ok = true
	}
	if ok {
		line = "remote: " + tui.Rate(up) + " ▲"
		if len(c.Down) > 0 {
			line += "  " + tui.Rate(down) + " ▼"
		}
	}
	b.Put(tui.Truncate(line, b.Width(), c.ASCII), 0, 0, c.Dim)
}

// scaleKey is the scale group a series draws against: its own group, or with
// Shared one group for all bytes and one for all packets. The shared names
// carry the chart's first group, so two charts that keep their easing state in
// one map do not share a scale.
func (c remoteChart) scaleKey(s remoteSeries) string {
	if !c.Shared {
		return s.Group
	}
	prefix := ""
	if len(c.Up) > 0 {
		prefix = c.Up[0].Group
	} else if len(c.Down) > 0 {
		prefix = c.Down[0].Group
	}
	if s.Packets {
		return prefix + "|shared packets"
	}
	return prefix + "|shared bytes"
}

// remoteScales folds the window peak of every scale group into its easing state
// and returns the scale each group is drawn against this frame. Groups are
// walked in sorted order so the easing never depends on map iteration.
func remoteScales(c remoteChart) map[string]float64 {
	peaks := map[string]float64{}
	for _, dir := range [][]remoteSeries{c.Up, c.Down} {
		for _, s := range dir {
			floor := float64(remoteByteFloor)
			if s.Packets {
				floor = remotePacketFloor
			}
			peak := remoteWindowPeak(c, s.Values)
			if c.dots > 0 {
				// The curve is drawn through bucket averages, so its height
				// follows the highest of those, as the app's follows its
				// highest sample. It is taken before the curve is blurred.
				// The blurred top of a sample that has just arrived is
				// wider than it is one reading later, and a scale that
				// followed it would swing and redraw the whole plot twice.
				// The peak label still names the highest sample.
				// A short zoom scales to the last minute at least. Ten
				// readings of bursty traffic change their highest value
				// almost every second, and the scale, and with it the whole
				// plot, would follow.
				peak = 0
				wide := c
				wide.Window = max(c.Window, remoteScaleMinWindow)
				for _, p := range remotePointsOf(wide, s.Values, c.dots) {
					peak = max(peak, p.v)
				}
			}
			peak = max(peak, floor)
			if key := c.scaleKey(s); peak > peaks[key] {
				peaks[key] = peak
			}
		}
	}
	if c.Scales == nil {
		// No state to ease: draw at the window peak, held away from zero.
		out := map[string]float64{}
		for g, peak := range peaks {
			out[g] = max(peak, 1)
		}
		return out
	}
	out := map[string]float64{}
	groups := make([]string, 0, len(peaks))
	for g := range peaks {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		s := c.Scales[g]
		if s == nil {
			s = &remoteScale{}
			c.Scales[g] = s
		}
		s.target(peaks[g], c.Now)
		out[g] = s.valueAt(c.Now)
	}
	return out
}

// remoteWindowPeak is the largest sample the plot shows for a series.
func remoteWindowPeak(c remoteChart, vals []float64) float64 {
	peak := 0.0
	for i := len(vals) - 1; i >= 0; i-- {
		if time.Duration(len(vals)-1-i)*c.Interval > c.Window {
			break
		}
		if v := vals[i]; v > peak && !math.IsNaN(v) && !math.IsInf(v, 0) {
			peak = v
		}
	}
	return peak
}

// remoteAverage is the corner figure: the mean of the newest buckets, which is
// what the client app prints above and below the chart.
func remoteAverage(vals []float64) float64 {
	sum, n := 0.0, 0
	for i := len(vals) - 1; i >= 0 && n < remoteAverageBuckets; i-- {
		if math.IsNaN(vals[i]) || math.IsInf(vals[i], 0) {
			continue
		}
		sum += max(vals[i], 0)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// remoteNewest is the newest sample of a direction, for the one-line summary.
func remoteNewest(dir []remoteSeries) float64 {
	if len(dir) == 0 {
		return 0
	}
	return remoteAverage(dir[0].Values)
}

// drawLegend prints the direction's series names on the row and gives the rest
// of the row to the averages. The figures come first: when both do not fit side
// by side, the legend is dropped rather than the numbers. Empty labels are
// skipped so a packet series with no name does not clutter the row.
func (c remoteChart) drawLegend(b *tui.Buffer, row int, dir []remoteSeries, arrow rune) {
	if row < 0 || len(dir) == 0 {
		return
	}
	labels := make([]string, 0, len(dir))
	for _, s := range dir {
		if s.Label == "" {
			continue
		}
		labels = append(labels, s.Label)
	}
	if len(labels) == 0 {
		return
	}
	text := strings.Join(labels, " ") + " " + string(arrow)
	b.Put(tui.Truncate(text, b.Width(), c.ASCII), 0, row, c.Dim)
}

// remoteSegment is one piece of the corner figures: a formatted average in its
// series color, or the direction arrow. packets marks a packet rate, which is
// dropped before the bytes rate when the row is too narrow for both.
type remoteSegment struct {
	text    string
	style   tui.Style
	packets bool
}

// averageSegments builds the corner figures for one direction: the newest
// average of each series, then the arrow, which is bright when the direction
// carries traffic and muted when it does not.
func (c remoteChart) averageSegments(dir []remoteSeries, arrow rune) ([]remoteSegment, bool) {
	segs := make([]remoteSegment, 0, len(dir)+1)
	active := false
	for _, s := range dir {
		avg := remoteAverage(s.Values)
		st := s.Style
		if s.Packets {
			if avg <= 0 {
				st = c.Dim
			} else {
				active = true
			}
			segs = append(segs, remoteSegment{text: tui.PacketRate(avg), style: st, packets: true})
			continue
		}
		if avg <= 0 {
			st = c.Dim
		} else {
			active = true
		}
		segs = append(segs, remoteSegment{text: tui.Rate(avg), style: st})
	}
	st := c.Dim
	if active {
		st = c.Text
	}
	return append(segs, remoteSegment{text: string(arrow), style: st}), active
}

// segmentsWidth is how many cells a run of segments occupies, gap included.
func segmentsWidth(segs []remoteSegment) int {
	width := 0
	for i, sg := range segs {
		if i > 0 {
			width += 2
		}
		width += tui.StringWidth(sg.text)
	}
	return width
}

// putSegments draws the segments from x.
func (c remoteChart) putSegments(b *tui.Buffer, row, x int, segs []remoteSegment) {
	for i, sg := range segs {
		if i > 0 {
			x = b.Put("  ", x, row, tui.Style{})
		}
		x = b.Put(tui.Truncate(sg.text, max(b.Width()-x, 0), c.ASCII), x, row, sg.style)
	}
}

// drawAverages prints the corner figures right-aligned, dropping the legend and
// then the leftmost figures when the row cannot hold everything.
func (c remoteChart) drawAverages(b *tui.Buffer, row int, dir []remoteSeries, arrow rune) {
	if row < 0 || len(dir) == 0 {
		return
	}
	segs, _ := c.averageSegments(dir, arrow)
	width := segmentsWidth(segs)
	if width <= b.Width() {
		if legend := c.legendWidth(dir); legend+1+width <= b.Width() {
			c.drawLegend(b, row, dir, arrow)
		}
		c.putSegments(b, row, b.Width()-width, segs)
		return
	}
	// Narrower than the figures: drop the packet figure first, then the bytes
	// figure. The bytes reading is the primary one and the arrow is the last to
	// go, since that pair still says which way the traffic goes.
	for len(segs) > 2 && width > b.Width() {
		drop := 0
		for i, sg := range segs[:len(segs)-1] {
			if sg.packets {
				drop = i
				break
			}
		}
		width -= tui.StringWidth(segs[drop].text) + 2
		segs = append(segs[:drop], segs[drop+1:]...)
	}
	c.putSegments(b, row, max(b.Width()-width, 0), segs)
}

// legendWidth is how many cells a direction's legend occupies.
func (c remoteChart) legendWidth(dir []remoteSeries) int {
	labels := make([]string, 0, len(dir))
	for _, s := range dir {
		if s.Label == "" {
			continue
		}
		labels = append(labels, s.Label)
	}
	if len(labels) == 0 {
		return 0
	}
	return tui.StringWidth(strings.Join(labels, " ")) + 2
}

// drawHalfASCIIAll draws every curve of one half with characters, for
// terminals without braille. A packet series with no positive sample draws
// nothing: its corner figure still says so, muted.
func (c remoteChart) drawHalfASCIIAll(b *tui.Buffer, l remoteChartLayout, dir []remoteSeries, up bool, scales map[string]float64) {
	y0, rows := l.upY, l.upRows
	if !up {
		y0, rows = l.downY, l.downRows
	}
	if rows < 1 || len(dir) == 0 {
		return
	}
	// Draw from the back so the first series (the byte curve) lands on top
	// of the packet curve in the cells they share.
	for i := len(dir) - 1; i >= 0; i-- {
		s := dir[i]
		if s.Packets && !anyPositive(s.Values) {
			continue
		}
		pts := remotePlotPoints(c, s.Values, 2*b.Width())
		if len(pts) == 0 {
			continue
		}
		c.drawHalfASCII(b, s, pts, scales[c.scaleKey(s)], up, y0, rows, i)
	}
}

// drawPlot draws both halves and the baseline into one braille canvas that
// spans the up rows, the center row and the down rows. The baseline is a dot
// row of the center row, so a curve starts on it and an idle stretch lies on
// it, as in the client app.
func (c remoteChart) drawPlot(b *tui.Buffer, l remoteChartLayout, scales map[string]float64) {
	rows := l.upRows + 1 + l.downRows
	if rows < 1 {
		return
	}
	all := append(append([]remoteSeries{}, c.Up...), c.Down...)
	cv := newRemoteCanvas(b.Width(), rows, l.upRows*4+1, all, c.Dim)
	for half, dir := range [][]remoteSeries{c.Up, c.Down} {
		offset := 0
		if half == 1 {
			offset = len(c.Up)
		}
		// The byte series is first and must stay readable, so it is plotted
		// last and wins the cells where a packet curve crosses it.
		for i := len(dir) - 1; i >= 0; i-- {
			s := dir[i]
			if s.Packets && !anyPositive(s.Values) {
				continue
			}
			pts := remotePlotPoints(c, s.Values, 2*b.Width())
			if len(pts) == 0 {
				continue
			}
			cv.plot(pts, scales[c.scaleKey(s)], half == 0, offset+i)
		}
	}
	cv.draw(b, l.upY)
}

// anyPositive reports whether a series carries at least one positive sample.
func anyPositive(vals []float64) bool {
	for _, v := range vals {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

// drawHalfASCII is the fallback for terminals without braille: one column per
// cell, the curve drawn as a run of glyphs from the center outward.
func (c remoteChart) drawHalfASCII(b *tui.Buffer, s remoteSeries, pts []remotePoint, scale float64, up bool, y0, rows, idx int) {
	if scale <= 0 {
		scale = 1
	}
	glyph := s.Glyph
	if glyph == 0 {
		glyph = []rune{'#', '+', '*', 'o'}[idx%4]
	}
	for x := 0; x < b.Width(); x++ {
		frac := max(0, remoteMonotoneAt(pts, float64(2*x))) / scale
		h := int(math.Round(min(frac, 1) * float64(rows)))
		h = min(max(h, 0), rows)
		for i := 0; i < h; i++ {
			y := y0 + rows - 1 - i
			if !up {
				y = y0 + i
			}
			b.Set(x, y, tui.Cell{Rune: glyph, Style: s.Style})
		}
	}
}

// drawPeak labels the window peak of the direction's first series. The label
// slides sideways with the peak's column and stays inside the plot.
func (c remoteChart) drawPeak(b *tui.Buffer, dir []remoteSeries, up bool, row int) {
	if row < 0 || len(dir) == 0 {
		return
	}
	pts := remotePointsIn(c, dir[0].Values, 2*b.Width(), math.MaxInt, false)
	if len(pts) == 0 {
		return
	}
	peak, at := 0.0, 0.0
	for _, p := range pts {
		if p.v > peak {
			peak, at = p.v, p.x
		}
	}
	if peak <= 0 {
		return
	}
	arrow := '▲'
	if !up {
		arrow = '▼'
	}
	text := tui.Rate(peak) + " " + string(arrow)
	width := tui.StringWidth(text)
	if width >= b.Width() {
		b.Put(tui.Truncate(text, b.Width(), c.ASCII), 0, row, c.Dim)
		return
	}
	x := int(math.Round(at / 2))
	x = min(max(x-width/2, 0), b.Width()-width)
	b.Put(text, x, row, c.Dim)
}

// remoteCanvas collects the dots of the plot: both halves and the baseline
// dot row between them. A cell can carry only one color, so a curve wins over
// the baseline, and a later series wins over an earlier one in
// the few cells where two curves cross. The baseline dots are kept in a curve
// cell, so the line stays whole under a curve.
type remoteCanvas struct {
	cells, rows int
	base        int   // dot row of the baseline
	curve       []int // series index + 1; 0 is empty
	styles      []tui.Style
	baseStyle   tui.Style
}

func newRemoteCanvas(cells, rows, base int, series []remoteSeries, baseStyle tui.Style) *remoteCanvas {
	cv := &remoteCanvas{cells: cells, rows: rows, base: base, curve: make([]int, cells*2*rows*4), baseStyle: baseStyle}
	cv.styles = make([]tui.Style, len(series))
	for i, s := range series {
		cv.styles[i] = s.Style
	}
	return cv
}

func (cv *remoteCanvas) setCurve(x, y, idx int) {
	if x < 0 || y < 0 || x >= cv.cells*2 || y >= cv.rows*4 || y == cv.base {
		return
	}
	cv.curve[y*cv.cells*2+x] = idx + 1
}

// plot draws one curve, one dot thick: the value at every dot column, with the
// gap between neighboring columns joined so the line has no breaks. A zero
// lies on the baseline, and a rise or a fall starts from it. Nothing is
// drawn under the curve: a dot pattern there reads as static in a terminal.
func (cv *remoteCanvas) plot(pts []remotePoint, scale float64, up bool, idx int) {
	if scale <= 0 {
		scale = 1
	}
	maxH := cv.base
	if !up {
		maxH = cv.rows*4 - 1 - cv.base
	}
	if maxH < 1 {
		return
	}
	vals := remoteSmoothColumns(pts, cv.cells*2)
	prev, prevH := cv.base, 0
	for x := 0; x < cv.cells*2; x++ {
		frac := vals[x] / scale
		// A curve drawn while an ease is still under it can exceed its half;
		// clamping the fraction keeps the conversion to an int well defined.
		// The height rounds down, so a rate under one dot lies on the
		// baseline the way a quiet stretch does in the app.
		h := int(math.Floor(min(frac, 1) * float64(maxH)))
		h = min(max(h, 0), maxH)
		y := cv.base - h
		if !up {
			y = cv.base + h
		}
		if h == 0 && prevH == 0 {
			prev = cv.base
			continue
		}
		if x == 0 {
			// The window cuts the history at the left edge: the curve starts
			// there at its own height instead of rising from the baseline.
			prev = y
		}

		a, z := min(prev, y), max(prev, y)
		for yy := a; yy <= z; yy++ {
			cv.setCurve(x, yy, idx)
		}
		prev, prevH = y, h
	}
}

// remoteSmoothColumns is the curve's value at every dot column. The samples
// are joined with a monotone cubic (Fritsch and Carlson), which passes through
// every sample, never overshoots past a peak or below zero, and is flat at
// every local peak and dip, so a burst rounds off at the top and tapers at the
// foot. The app draws its curve through the samples as they are, and so does
// this. A blur on top would lower every peak below the scale, and a scale that
// followed the blurred top would swing each time a new reading changed the
// blur at the right edge. The peak label names the highest sample.
func remoteSmoothColumns(pts []remotePoint, dots int) []float64 {
	raw := make([]float64, dots)
	top := 0.0
	for _, p := range pts {
		top = max(top, p.v)
	}
	for x := range raw {
		raw[x] = min(max(0, remoteSplineAt(pts, float64(x))), top)
	}
	return raw
}

// remoteSplineAt is the Catmull-Rom spline through pts at dot column x, held
// flat outside the sampled range. Its tangent at a point follows both
// neighbors, so a run of a few points reads as one smooth curve instead of a
// step at every point. The caller clamps it to zero and the highest point.
func remoteSplineAt(pts []remotePoint, x float64) float64 {
	n := len(pts)
	switch {
	case n == 0:
		return 0
	case n == 1 || x <= pts[0].x:
		return pts[0].v
	case x >= pts[n-1].x:
		return pts[n-1].v
	}
	i := sort.Search(n-1, func(k int) bool { return pts[k+1].x >= x })
	if i >= n-1 {
		i = n - 2
	}
	h := pts[i+1].x - pts[i].x
	if h <= 0 {
		return pts[i].v
	}
	at := func(k int) remotePoint { return pts[min(max(k, 0), n-1)] }
	tangent := func(k int) float64 {
		a, b := at(k-1), at(k+1)
		if d := b.x - a.x; d > 0 {
			return (b.v - a.v) / d
		}
		return 0
	}
	t := (x - pts[i].x) / h
	m0, m1 := tangent(i)*h, tangent(i+1)*h
	t2, t3 := t*t, t*t*t
	return (2*t3-3*t2+1)*pts[i].v + (t3-2*t2+t)*m0 + (-2*t3+3*t2)*pts[i+1].v + (t3-t2)*m1
}

// remoteMonotoneAt is the monotone cubic through pts at dot column x, held
// flat outside the sampled range.
func remoteMonotoneAt(pts []remotePoint, x float64) float64 {
	n := len(pts)
	switch {
	case n == 0:
		return 0
	case n == 1 || x <= pts[0].x:
		return pts[0].v
	case x >= pts[n-1].x:
		return pts[n-1].v
	}
	i := sort.Search(n-1, func(k int) bool { return pts[k+1].x >= x })
	if i >= n-1 {
		i = n - 2
	}
	slope := func(k int) float64 {
		if d := pts[k+1].x - pts[k].x; d > 0 {
			return (pts[k+1].v - pts[k].v) / d
		}
		return 0
	}
	tangent := func(k int) float64 {
		switch {
		case k == 0:
			return slope(0)
		case k == n-1:
			return slope(n - 2)
		}
		a, b := slope(k-1), slope(k)
		if a*b <= 0 {
			return 0 // a peak, a dip or a flat stretch: the curve is level here
		}
		// The harmonic mean of the two slopes keeps the cubic monotone
		// between samples (Fritsch and Butland).
		return 2 / (1/a + 1/b)
	}
	h := pts[i+1].x - pts[i].x
	if h <= 0 {
		return pts[i].v
	}
	t := (x - pts[i].x) / h
	m0, m1 := tangent(i)*h, tangent(i+1)*h
	t2, t3 := t*t, t*t*t
	return (2*t3-3*t2+1)*pts[i].v + (t3-2*t2+t)*m0 + (-2*t3+3*t2)*pts[i+1].v + (t3-t2)*m1
}

// draw encodes the collected dots into cells: 2 by 4 dots per braille glyph.
func (cv *remoteCanvas) draw(b *tui.Buffer, y0 int) {
	for r := 0; r < cv.rows; r++ {
		for x := 0; x < cv.cells; x++ {
			var mask, baseMask rune
			var st tui.Style
			for k := 0; k < 4; k++ {
				y := r*4 + (3 - k) // k counts from the bottom dot of the cell
				if y == cv.base {
					baseMask |= remoteBrailleLeft[k] | remoteBrailleRight[k]
					continue
				}
				if idx := cv.curve[y*cv.cells*2+2*x]; idx != 0 {
					mask |= remoteBrailleLeft[k]
					st = cv.styles[idx-1]
				}
				if idx := cv.curve[y*cv.cells*2+2*x+1]; idx != 0 {
					mask |= remoteBrailleRight[k]
					st = cv.styles[idx-1]
				}
			}
			switch {
			case mask != 0:
				b.Set(x, y0+r, tui.Cell{Rune: 0x2800 + mask | baseMask, Style: st})
			case baseMask != 0:
				b.Set(x, y0+r, tui.Cell{Rune: 0x2800 + baseMask, Style: cv.baseStyle})
			}
		}
	}
}

// clock is the time the plot scrolls with: the dot aligned clock when
// drawRemoteChart set one, else Now.
func (c remoteChart) clock() time.Time {
	if !c.plotNow.IsZero() {
		return c.plotNow
	}
	return c.Now
}

// remoteDotClock moves now back to a multiple of one dot column of the window.
// The app moves its chart by a fraction of a pixel and anti-aliases it, so a
// sample just slides. A braille dot cannot be anywhere between two columns, so
// the clock moves one column at a time instead. A sample then keeps its
// distance from the grid, and the curve through the samples is the same curve
// shifted left, never sampled at a new phase.
func remoteDotClock(now time.Time, window time.Duration, dots int) time.Time {
	if dots < 2 || window <= 0 {
		return now
	}
	step := int64(window) / int64(dots-1)
	if step < 1 {
		return now
	}
	ns := now.UnixNano()
	return time.Unix(0, ns-ns%step)
}

// remotePointsOf places a series on the plot without easing the newest sample,
// used by scale calculation so a new peak targets its full height at once.
func remotePointsOf(c remoteChart, vals []float64, dots int) []remotePoint {
	return remotePointsIn(c, vals, dots, remoteDrawBuckets(dots), false)
}

// remotePlotPoints places a series for drawing, easing the newest sample from
// its previous value over remoteSampleEase so the right edge glides instead of
// hopping. Stored values are never modified.
func remotePlotPoints(c remoteChart, vals []float64, dots int) []remotePoint {
	return remotePointsIn(c, vals, dots, remoteDrawBuckets(dots), true)
}

// remoteDrawBuckets is how many points a curve is drawn through: one bucket for
// about three dot columns. A window with more samples than that is
// averaged into buckets, so a noisy one second series at 5m or 15m reads as
// smoothly as the 1m view, and the spline has room to round off between
// points.
func remoteDrawBuckets(dots int) int {
	return max(2, dots/3)
}

// remotePointsIn is remotePointsOf with an explicit bucket count and optional
// newest-sample easing. The peak label passes no limit and ease=false, so it
// names the highest real sample.
func remotePointsIn(c remoteChart, vals []float64, dots, buckets int, ease bool) []remotePoint {
	if dots < 2 || len(vals) == 0 || c.Window <= 0 || c.Interval <= 0 {
		return nil
	}
	now := c.clock()
	windowStart := now.Add(-c.Window)
	var samples []remoteSample
	stored := len(c.Times) == len(vals)
	for i := len(vals) - 1; i >= 0; i-- {
		at := c.Newest.Add(-time.Duration(len(vals)-1-i) * c.Interval)
		if stored {
			at = time.Unix(0, c.Times[i])
		}
		if at.Before(windowStart) {
			break
		}
		if math.IsNaN(vals[i]) || math.IsInf(vals[i], 0) {
			continue // a missed poll leaves the curve through its neighbours
		}
		samples = append(samples, remoteSample{at.UnixNano(), max(vals[i], 0)})
	}
	if len(samples) == 0 {
		return nil
	}
	for i, j := 0, len(samples)-1; i < j; i, j = i+1, j-1 {
		samples[i], samples[j] = samples[j], samples[i]
	}
	if ease && !c.Newest.IsZero() {
		if elapsed := c.Now.Sub(c.Newest); elapsed < remoteSampleEase {
			eased := remoteEaseProgress(elapsed, remoteSampleEase)
			last := len(samples) - 1
			prev := prevFiniteValue(vals)
			if last > 0 {
				prev = samples[last-1].v
			}
			samples[last].v = prev + (samples[last].v-prev)*eased
		}
	}
	if first := time.Unix(0, samples[0].at); first.After(windowStart) {
		var ramp []remoteSample
		for t := first.Add(-c.Interval); t.After(windowStart); t = t.Add(-c.Interval) {
			ramp = append(ramp, remoteSample{t.UnixNano(), 0})
		}
		for i, j := 0, len(ramp)-1; i < j; i, j = i+1, j-1 {
			ramp[i], ramp[j] = ramp[j], ramp[i]
		}
		samples = append(ramp, samples...)
	}
	if last := samples[len(samples)-1]; last.at < now.UnixNano() {
		samples = append(samples, remoteSample{now.UnixNano(), last.v})
	}
	if len(samples) > buckets {
		samples = remoteBucketize(samples, buckets, c.Window, windowStart.UnixNano())
	}
	pts := make([]remotePoint, len(samples))
	span := float64(c.Window)
	for i, s := range samples {
		x := float64(dots-1) * (1 - float64(now.UnixNano()-s.at)/span)
		pts[i] = remotePoint{x: min(max(x, 0), float64(dots-1)), v: s.v}
	}
	return pts
}

// remoteBucketize averages runs of samples into at most dots buckets aligned to
// absolute time. It averages, never sums or takes the maximum, so a bucket is
// the mean rate over its span. The oldest bucket is dropped when it straddles the window
// start, since it would shimmer as samples fall off the left edge.
func remoteBucketize(samples []remoteSample, dots int, window time.Duration, windowStart int64) []remoteSample {
	width := int64(window) / int64(dots)
	if width < 1 {
		return samples
	}
	out := make([]remoteSample, 0, dots)
	for i := 0; i < len(samples); {
		id := samples[i].at / width
		sum, n, at := 0.0, 0, 0.0
		start := i
		for i < len(samples) && samples[i].at/width == id {
			sum += samples[i].v
			at += float64(samples[i].at)
			n++
			i++
		}
		if start == 0 && windowStart/width == id {
			continue // partial oldest bucket
		}
		out = append(out, remoteSample{at: int64(at / float64(n)), v: sum / float64(n)})
	}
	return out
}
