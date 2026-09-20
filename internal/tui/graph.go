package tui

import (
	"fmt"
	"math"
)

// Braille dot bits for one cell, listed bottom row first so a column of n dots
// fills upward. A braille cell is 2 columns by 4 rows of dots, which gives the
// graph twice the horizontal and four times the vertical resolution of one
// glyph per cell.
var (
	brailleLeft  = [4]rune{0x40, 0x04, 0x02, 0x01}
	brailleRight = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// niceSteps are the mantissas an axis top may take. Ceiling to one of these
// keeps the scale from jumping to an unreadable value like 41.3.
var niceSteps = []float64{1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10}

func niceCeilDecimal(v float64) float64 {
	exp := math.Floor(math.Log10(v))
	scale := math.Pow(10, exp)
	f := v / scale
	for _, n := range niceSteps {
		// The tolerance absorbs Log10 landing a hair under an exact power.
		if n >= f*(1-1e-9) {
			return n * scale
		}
	}
	return 10 * scale
}

// niceCeil rounds v up to a round axis value. With binary set the rounding
// happens within the current power-of-1024 unit, so a byte axis tops out at
// 512 KiB or 2 MiB rather than 488.3 KiB.
func niceCeil(v float64, binary bool) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if !binary {
		return niceCeilDecimal(v)
	}
	unit := 1.0
	for v/unit >= 1024 {
		unit *= 1024
	}
	// The decimal steps run up to 10 * 10^k, which can overshoot the unit
	// boundary (1200 KiB rather than 1 MiB); stop at the boundary.
	return math.Min(niceCeilDecimal(v/unit), 1024) * unit
}

// Graph is a time series drawn as a filled braille area chart with a value
// axis down the left edge.
type Graph struct {
	// Samples are oldest first; the last is the newest and is drawn at the
	// right edge.
	Samples []float64
	// Max fixes the axis top. Zero scales to the largest sample, rounded up to
	// a round value.
	Max float64
	// Binary rounds an automatic axis top to powers of 1024 instead of 10, for
	// byte quantities.
	Binary bool
	// Format renders the axis labels. Nil uses %g.
	Format    func(float64) string
	Style     Style
	AxisStyle Style
}

// DrawGraph draws g into b and returns the value at the top of the axis (zero
// when the series is empty or all zero and there is no scale). Each
// cell holds two samples, so the newest 2*width samples fit; a longer series is
// averaged down into that many columns so the whole window stays visible, and a
// shorter one is right-aligned with the newest sample at the right edge. A
// sample of zero still draws a one-dot baseline, which is what tells "measured
// zero" from "no sample yet". An empty or all-zero series draws the baseline
// with only a zero label on the axis, since there is no scale to speak of.
func DrawGraph(b *Buffer, g Graph, ascii bool) float64 {
	w, h := b.Width(), b.Height()
	if w <= 0 || h <= 0 {
		return 0
	}
	format := g.Format
	if format == nil {
		format = func(v float64) string { return fmt.Sprintf("%g", v) }
	}

	samples := make([]float64, len(g.Samples))
	peak := 0.0
	for i, v := range g.Samples {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		samples[i] = v
		peak = math.Max(peak, v)
	}
	top := g.Max
	if top <= 0 {
		top = niceCeil(peak, g.Binary)
	}
	flat := top <= 0
	if flat {
		top = 1 // any scale will do for a baseline
	}
	shown := top
	if flat {
		shown = 0
	}

	// Axis labels on the top and bottom rows, and the middle when tall enough.
	labels := map[int]string{h - 1: format(0)}
	if !flat {
		labels[0] = format(top)
		if h >= 5 {
			labels[(h-1)/2] = format(top / 2)
		}
	}
	axis := 0
	for _, l := range labels {
		axis = max(axis, StringWidth(l))
	}
	x0 := axis + 1
	if w-x0 < 1 {
		// No room for a plot beside the axis: drop the axis.
		axis, x0, labels = 0, 0, nil
	}
	for y := 0; y < h; y++ {
		if x0 == 0 {
			break
		}
		tick := '│'
		if ascii {
			tick = '|'
		}
		if l, ok := labels[y]; ok {
			tick = '┤'
			if ascii {
				tick = '+'
			}
			b.Put(l, axis-StringWidth(l), y, g.AxisStyle)
		}
		b.Set(axis, y, Cell{Rune: tick, Style: g.AxisStyle})
	}

	plotW := w - x0
	cols := plotColumns(samples, 2*plotW)
	if ascii {
		drawGraphASCII(b.Sub(Rect{X: x0, Y: 0, W: plotW, H: h}), cols, top, g.Style)
		return shown
	}
	dots := 4 * h
	heights := make([]int, len(cols)) // dots per column, -1 for no sample
	for i, v := range cols {
		heights[i] = -1
		if !math.IsNaN(v) {
			heights[i] = min(max(int(math.Round(v/top*float64(dots))), 1), dots)
		}
	}
	for cx := 0; cx < plotW; cx++ {
		for row := 0; row < h; row++ {
			var mask rune
			base := (h - 1 - row) * 4 // dots below this cell row
			for k := 0; k < 4; k++ {
				if heights[2*cx] > base+k {
					mask |= brailleLeft[k]
				}
				if heights[2*cx+1] > base+k {
					mask |= brailleRight[k]
				}
			}
			if mask != 0 {
				b.Set(x0+cx, row, Cell{Rune: 0x2800 + mask, Style: g.Style})
			}
		}
	}
	return shown
}

// plotColumns lays samples onto exactly n columns: averaged into buckets when
// there are more samples than columns, right-aligned when fewer. Columns with
// no sample are NaN.
func plotColumns(samples []float64, n int) []float64 {
	cols := make([]float64, n)
	for i := range cols {
		cols[i] = math.NaN()
	}
	if len(samples) <= n {
		copy(cols[n-len(samples):], samples)
		return cols
	}
	for i := 0; i < n; i++ {
		lo := i * len(samples) / n
		hi := max((i+1)*len(samples)/n, lo+1)
		sum := 0.0
		for _, v := range samples[lo:hi] {
			sum += v
		}
		cols[i] = sum / float64(hi-lo)
	}
	return cols
}

// drawGraphASCII is the fallback for terminals without braille: one glyph per
// pair of sample columns, stacked '#' rows with the top row graded by the
// sparkline ramp, and a baseline glyph so a zero still shows.
func drawGraphASCII(b *Buffer, cols []float64, top float64, st Style) {
	h := b.Height()
	for cx := 0; cx < b.Width(); cx++ {
		sum, n := 0.0, 0
		for _, v := range cols[2*cx : 2*cx+2] {
			if !math.IsNaN(v) {
				sum += v
				n++
			}
		}
		if n == 0 {
			continue
		}
		rem := min(max(int(math.Round(sum/float64(n)/top*float64(h*8))), 1), h*8)
		for row := h - 1; row >= 0 && rem > 0; row-- {
			take := min(rem, 8)
			b.Set(cx, row, Cell{Rune: sparkASCII[take-1], Style: st})
			rem -= take
		}
	}
}
