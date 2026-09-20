package tui

import "math"

var barPartials = []rune("▏▎▍▌▋▊▉")

// Bar is a horizontal gauge on one row: a label, the gauge, and a value text,
//
//	heap  ███████░░░ 1.8/4.0 GiB
//
// The gauge takes whatever the label and value leave, in eighths of a cell so
// small changes still move it.
type Bar struct {
	Label string
	Value string
	// Frac is the fill, clamped to 0..1 (NaN counts as 0).
	Frac float64
	// LabelWidth pads the label to a fixed width so several bars stack into
	// aligned gauges. Zero uses the label's own width.
	LabelWidth int
	LabelStyle Style
	ValueStyle Style
	Fill       Style
	Empty      Style
}

// DrawBar draws the bar on row 0 of b. When the row is too narrow for a
// gauge of at least one cell beside the label and value it drops the value
// first, then the gauge, and finally truncates the label, so a squeezed panel
// still says something true rather than drawing a broken bar.
func DrawBar(b *Buffer, bar Bar, ascii bool) {
	w := b.Width()
	if w <= 0 || b.Height() <= 0 {
		return
	}
	lw := max(bar.LabelWidth, StringWidth(bar.Label))
	label := Truncate(bar.Label, w, ascii)
	if bar.Label != "" {
		b.Put(label, 0, 0, bar.LabelStyle)
	}
	x := 0
	if bar.Label != "" || bar.LabelWidth > 0 {
		x = lw + 1
	}
	value := bar.Value
	vw := 0
	if value != "" {
		vw = StringWidth(value) + 1
	}
	gauge := w - x - vw
	if gauge < 1 {
		// Value first, then the gauge itself.
		value, vw = "", 0
		gauge = w - x
	}
	if gauge < 1 {
		return
	}

	frac := bar.Frac
	if math.IsNaN(frac) || frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	full, part := gaugeCells(frac, gauge, ascii)
	fullRune, emptyRune := '█', '░'
	if ascii {
		fullRune, emptyRune = '#', '-'
	}
	for i := 0; i < gauge; i++ {
		switch {
		case i < full:
			b.Set(x+i, 0, Cell{Rune: fullRune, Style: bar.Fill})
		case i == full && part > 0:
			b.Set(x+i, 0, Cell{Rune: barPartials[part-1], Style: bar.Fill})
		default:
			b.Set(x+i, 0, Cell{Rune: emptyRune, Style: bar.Empty})
		}
	}
	if value != "" {
		b.Put(value, x+gauge+1, 0, bar.ValueStyle)
	}
}

// gaugeCells converts a fraction into whole cells plus a partial glyph index
// (0 for none, else 1..7 eighths). ASCII has no partial glyph, so it rounds to
// the nearest whole cell instead.
func gaugeCells(frac float64, width int, ascii bool) (full, part int) {
	if ascii {
		return int(math.Round(frac * float64(width))), 0
	}
	eighths := int(math.Round(frac * float64(width) * 8))
	return eighths / 8, eighths % 8
}
