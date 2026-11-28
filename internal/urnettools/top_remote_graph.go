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
// below it, each as a curve through the samples, with a faint fill under it and
// the peak value labelled where it happened. The newest data is on the right
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
	// remoteAverageBuckets is how many of the newest samples the corner
	// average covers.
	remoteAverageBuckets = 5
	// remoteDefaultWindow is how much time the plot spans by default.
	remoteDefaultWindow = 60 * time.Second
	// remoteMinWidth is the narrowest plot that still reads as a chart.
	remoteMinWidth = 8
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
// OverlapGlyph optionally marks shared ASCII cells with another series.
// Packets marks a packet rate series, formatted in pkt/s rather than bytes per
// second in the corner figures.
type remoteSeries struct {
	Label        string
	Style        tui.Style
	Group        string
	Glyph        rune
	OverlapGlyph rune
	Packets      bool
	Values       []float64
}

// remoteChart is one frame of the mirrored chart. Now is the clock the plot
// scrolls with, Newest the time of each series' newest sample. Scales holds the
// easing state per group, created on first use; the renderer folds the window
// peak into it, so the same map is passed again next frame.
type remoteChart struct {
	Up, Down []remoteSeries
	Now      time.Time
	Newest   time.Time
	Interval time.Duration
	Window   time.Duration
	Scales   map[string]*remoteScale
	ASCII    bool
	Dim      tui.Style // center line, legends and the peak labels
	Text     tui.Style // the direction arrows when a direction is active
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
	if peak == s.to {
		return
	}
	s.from, s.to, s.start = s.valueAt(now), peak, now
}

// valueAt is the eased scale at now.
func (s *remoteScale) valueAt(now time.Time) float64 {
	if !s.set {
		return 1
	}
	progress := float64(now.Sub(s.start)) / float64(remoteScaleEase)
	progress = min(max(progress, 0), 1)
	eased := 1 - math.Pow(1-progress, 3) // ease out cubic
	return s.from + (s.to-s.from)*eased
}

// remoteLayout places the parts of one frame: the legends and the up averages
// on the first row, the up plot, the center line, the down plot and the down
// averages on the last row. A peak label row is reserved inside a half when the
// half is tall enough, so a label never sits on the curve.
type remoteChartLayout struct {
	header, footer, center int
	upY, upRows            int
	downY, downRows        int
	upPeak, downPeak       int // -1 when the half has no room for a label
}

func remoteLayout(h int) remoteChartLayout {
	l := remoteChartLayout{header: 0, footer: h - 1, upPeak: -1, downPeak: -1}
	if h < 3 {
		l.footer = -1
		return l
	}
	plotRows := h - 3
	l.upRows = (plotRows + 1) / 2
	l.downRows = plotRows - l.upRows
	l.center = 1 + l.upRows
	l.upY = 1
	l.downY = l.center + 1
	if l.upRows >= 2 {
		l.upPeak, l.upY, l.upRows = l.upY, l.upY+1, l.upRows-1
	}
	if l.downRows >= 2 {
		l.downPeak = l.downY + l.downRows - 1
		l.downRows--
	}
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
	scales := remoteScales(c)
	l := remoteLayout(h)

	if len(c.Up) == 0 && len(c.Down) == 0 {
		b.Put(tui.Truncate("remote: waiting for traffic", w, c.ASCII), 0, l.header, c.Dim)
		return
	}
	c.drawAverages(b, l.header, c.Up, '▲')
	for x := 0; x < w; x++ {
		ch := '─'
		if c.ASCII {
			ch = '-'
		}
		b.Set(x, l.center, tui.Cell{Rune: ch, Style: c.Dim})
	}
	c.drawHalf(b, l, c.Up, true, scales)
	c.drawHalf(b, l, c.Down, false, scales)
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

// remoteScales folds the window peak of every scale group into its easing state
// and returns the scale each group is drawn against this frame. Groups are
// walked in sorted order so the easing never depends on map iteration.
func remoteScales(c remoteChart) map[string]float64 {
	peaks := map[string]float64{}
	for _, dir := range [][]remoteSeries{c.Up, c.Down} {
		for _, s := range dir {
			peak := remoteWindowPeak(c, s.Values)
			if peak > peaks[s.Group] {
				peaks[s.Group] = peak
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

// drawHalf draws every curve of one half into the rows the layout gave it.
// A packet series with no positive sample draws nothing: its corner figure
// still says so, muted, and the half keeps its bytes-only picture.
func (c remoteChart) drawHalf(b *tui.Buffer, l remoteChartLayout, dir []remoteSeries, up bool, scales map[string]float64) {
	y0, rows := l.upY, l.upRows
	if !up {
		y0, rows = l.downY, l.downRows
	}
	if rows < 1 || len(dir) == 0 {
		return
	}
	if c.ASCII {
		// Draw from the back so the first series (the byte curve) lands on top
		// of the packet curve in the cells they share.
		for i := len(dir) - 1; i >= 0; i-- {
			s := dir[i]
			if s.Packets && !anyPositive(s.Values) {
				continue
			}
			pts := remotePointsOf(c, s.Values, 2*b.Width())
			if len(pts) == 0 {
				continue
			}
			c.drawHalfASCII(b, s, pts, scales[s.Group], up, y0, rows, i, dir)
		}
		return
	}
	cv := newRemoteCanvas(b.Width(), rows, dir)
	// The byte series is first and must stay readable, so it is plotted last
	// and wins the cells where a packet curve crosses it.
	for i := len(dir) - 1; i >= 0; i-- {
		s := dir[i]
		if s.Packets && !anyPositive(s.Values) {
			continue
		}
		pts := remotePointsOf(c, s.Values, 2*b.Width())
		if len(pts) == 0 {
			continue
		}
		cv.plot(pts, scales[s.Group], up, i)
	}
	cv.draw(b, y0)
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
func (c remoteChart) drawHalfASCII(b *tui.Buffer, s remoteSeries, pts []remotePoint, scale float64, up bool, y0, rows, idx int, dir []remoteSeries) {
	if scale <= 0 {
		scale = 1
	}
	glyph := s.Glyph
	if glyph == 0 {
		glyph = []rune{'#', '+', '*', 'o'}[idx%4]
	}
	for x := 0; x < b.Width(); x++ {
		frac := max(0, remoteCurveAt(pts, float64(2*x))) / scale
		h := int(math.Round(min(frac, 1) * float64(rows)))
		h = min(max(h, 0), rows)
		for i := 0; i < h; i++ {
			y := y0 + rows - 1 - i
			if !up {
				y = y0 + i
			}
			cellGlyph := glyph
			if s.OverlapGlyph != 0 {
				old := b.Cell(x, y).Rune
				for j := idx + 1; j < len(dir); j++ {
					other := dir[j].Glyph
					if other == 0 {
						other = []rune{'#', '+', '*', 'o'}[j%4]
					}
					if (old == other && other != glyph) || old == s.OverlapGlyph {
						cellGlyph = s.OverlapGlyph
						break
					}
				}
			}
			b.Set(x, y, tui.Cell{Rune: cellGlyph, Style: s.Style})
		}
	}
}

// drawPeak labels the window peak of a half. Every series with a peak gets its
// figure in the label, in series order, at the column where the first of them
// peaked: two curves that move together still print both figures without one
// label covering the other. The label slides sideways with the peak's column
// and stays inside the plot.
func (c remoteChart) drawPeak(b *tui.Buffer, dir []remoteSeries, up bool, row int) {
	if row < 0 || len(dir) == 0 {
		return
	}
	arrow := '▲'
	if !up {
		arrow = '▼'
	}
	figures := make([]string, 0, len(dir))
	at := 0.0
	for _, s := range dir {
		// Packet rates have a separate scale and are not byte-rate peaks.
		if s.Packets {
			continue
		}
		pts := remotePointsOf(c, s.Values, 2*b.Width())
		if len(pts) == 0 {
			continue
		}
		peak, x := 0.0, 0.0
		for _, p := range pts {
			if p.v > peak {
				peak, x = p.v, p.x
			}
		}
		if peak <= 0 {
			continue
		}
		if len(figures) == 0 {
			at = x
		}
		figures = append(figures, tui.Rate(peak))
	}
	if len(figures) == 0 {
		return
	}
	text := strings.Join(figures, " ") + " " + string(arrow)
	width := tui.StringWidth(text)
	if width >= b.Width() {
		b.Put(tui.Truncate(text, b.Width(), c.ASCII), 0, row, c.Dim)
		return
	}
	x := int(math.Round(at / 2))
	x = min(max(x-width/2, 0), b.Width()-width)
	b.Put(text, x, row, c.Dim)
}

// remoteCanvas collects the dots of one half. A cell can carry only one color,
// so the curve wins over the fill and a later series wins over an earlier one
// in the few cells where two curves cross.
type remoteCanvas struct {
	cells, rows int
	curve, fill []int // series index + 1; 0 is empty
	styles      []tui.Style
}

func newRemoteCanvas(cells, rows int, dir []remoteSeries) *remoteCanvas {
	cv := &remoteCanvas{cells: cells, rows: rows, curve: make([]int, cells*2*rows*4), fill: make([]int, cells*2*rows*4)}
	cv.styles = make([]tui.Style, len(dir))
	for i, s := range dir {
		cv.styles[i] = s.Style
	}
	return cv
}

func (cv *remoteCanvas) setCurve(x, y, idx int) {
	if x < 0 || y < 0 || x >= cv.cells*2 || y >= cv.rows*4 {
		return
	}
	cv.curve[y*cv.cells*2+x] = idx + 1
}

func (cv *remoteCanvas) setFill(x, y, idx int) {
	if x < 0 || y < 0 || x >= cv.cells*2 || y >= cv.rows*4 {
		return
	}
	cv.fill[y*cv.cells*2+x] = idx + 1
}

// plot draws one curve: the value at every dot column, the gap between
// neighbouring columns filled so the line has no breaks, and a stippled fill
// between the curve and the center line.
func (cv *remoteCanvas) plot(pts []remotePoint, scale float64, up bool, idx int) {
	if scale <= 0 {
		scale = 1
	}
	maxH := float64(cv.rows*4 - 1)
	prev := -1
	for x := 0; x < cv.cells*2; x++ {
		frac := max(0, remoteCurveAt(pts, float64(x))) / scale
		// A curve drawn while an ease is still under it can exceed its half;
		// clamping the fraction keeps the conversion to an int well defined.
		h := int(math.Round(min(frac, 1) * maxH))
		h = min(max(h, 0), cv.rows*4-1)
		var y, base int
		if up {
			base, y = cv.rows*4-1, cv.rows*4-1-h
		} else {
			base, y = 0, h
		}
		var lo, hi int
		if up {
			lo, hi = y+1, base
		} else {
			lo, hi = base, y-1
		}
		for yy := lo; yy <= hi; yy++ {
			// A diagonal stipple, one dot in three: a wash under the curve that
			// reads as a filled area without competing with the line.
			if (x+yy)%3 == 0 {
				cv.setFill(x, yy, idx)
			}
		}
		a, z := y, y
		if prev >= 0 {
			a, z = min(prev, y), max(prev, y)
		}
		for yy := a; yy <= z; yy++ {
			cv.setCurve(x, yy, idx)
		}
		prev = y
	}
}

// draw encodes the collected dots into cells: 2 by 4 dots per braille glyph.
func (cv *remoteCanvas) draw(b *tui.Buffer, y0 int) {
	for r := 0; r < cv.rows; r++ {
		for x := 0; x < cv.cells; x++ {
			var mask, fillMask rune
			var st, fst tui.Style
			for k := 0; k < 4; k++ {
				y := r*4 + (3 - k) // k counts from the bottom dot of the cell
				if idx := cv.curve[y*cv.cells*2+2*x]; idx != 0 {
					mask |= remoteBrailleLeft[k]
					st = cv.styles[idx-1]
				}
				if idx := cv.curve[y*cv.cells*2+2*x+1]; idx != 0 {
					mask |= remoteBrailleRight[k]
					st = cv.styles[idx-1]
				}
				if idx := cv.fill[y*cv.cells*2+2*x]; idx != 0 && mask == 0 {
					fillMask |= remoteBrailleLeft[k]
					fst = cv.styles[idx-1]
				}
				if idx := cv.fill[y*cv.cells*2+2*x+1]; idx != 0 && mask == 0 {
					fillMask |= remoteBrailleRight[k]
					fst = cv.styles[idx-1]
				}
			}
			switch {
			case mask != 0:
				b.Set(x, y0+r, tui.Cell{Rune: 0x2800 + mask, Style: st})
			case fillMask != 0:
				fst.Attrs |= tui.AttrDim
				b.Set(x, y0+r, tui.Cell{Rune: 0x2800 + fillMask, Style: fst})
			}
		}
	}
}

// remotePointsOf places a series on the plot: samples inside the window, padded
// with zeros back to the window start so the curve ramps in from the baseline,
// held out to the clock, and bucketed by absolute time when there are more
// samples than dot columns so a completed column never changes as time moves.
func remotePointsOf(c remoteChart, vals []float64, dots int) []remotePoint {
	if dots < 2 || len(vals) == 0 || c.Window <= 0 || c.Interval <= 0 {
		return nil
	}
	windowStart := c.Now.Add(-c.Window)
	var samples []remoteSample
	for i := len(vals) - 1; i >= 0; i-- {
		at := c.Newest.Add(-time.Duration(len(vals)-1-i) * c.Interval)
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
	if last := samples[len(samples)-1]; last.at < c.Now.UnixNano() {
		samples = append(samples, remoteSample{c.Now.UnixNano(), last.v})
	}
	if len(samples) > dots {
		samples = remoteBucketize(samples, dots, c.Window, windowStart.UnixNano())
	}
	pts := make([]remotePoint, len(samples))
	span := float64(c.Window)
	for i, s := range samples {
		x := float64(dots-1) * (1 - float64(c.Now.UnixNano()-s.at)/span)
		pts[i] = remotePoint{x: min(max(x, 0), float64(dots-1)), v: s.v}
	}
	return pts
}

// remoteBucketize averages runs of samples into at most dots buckets aligned to
// absolute time. The oldest bucket is dropped when it straddles the window
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

// remoteCurveAt is the smoothed value at dot column x: Catmull-Rom between the
// two samples either side of it, held flat outside the sampled range.
func remoteCurveAt(pts []remotePoint, x float64) float64 {
	switch {
	case len(pts) == 0:
		return 0
	case len(pts) == 1:
		return pts[0].v
	case x <= pts[0].x:
		return pts[0].v
	case x >= pts[len(pts)-1].x:
		return pts[len(pts)-1].v
	}
	i := sort.Search(len(pts)-1, func(k int) bool { return pts[k+1].x >= x })
	if i >= len(pts)-1 {
		i = len(pts) - 2
	}
	f := 0.0
	if d := pts[i+1].x - pts[i].x; d > 0 {
		f = (x - pts[i].x) / d
	}
	return remoteCatmull(pts, i, f)
}

// remoteCatmull is the Catmull-Rom value between pts[i] and pts[i+1] at
// fraction f.
func remoteCatmull(pts []remotePoint, i int, f float64) float64 {
	at := func(k int) float64 { return pts[min(max(k, 0), len(pts)-1)].v }
	p0, p1, p2, p3 := at(i-1), at(i), at(i+1), at(i+2)
	return 0.5 * (2*p1 + (-p0+p2)*f + (2*p0-5*p1+4*p2-p3)*f*f + (-p0+3*p1-3*p2+p3)*f*f*f)
}
