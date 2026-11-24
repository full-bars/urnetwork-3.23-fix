package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// brailleRows lists the rows of b that carry at least one braille dot.
func brailleRows(b *tui.Buffer) []int {
	var rows []int
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			if r := b.Cell(x, y).Rune; r > 0x2800 && r <= 0x28ff {
				rows = append(rows, y)
				break
			}
		}
	}
	return rows
}

// The plot fills the panel height for frames of 9 rows or more: each half gets
// (h-5)/2 rows, both corner figures sit on the top rows, and a peaked series
// touches the first and last plot rows.
func TestRemotePlotFillsPanel(t *testing.T) {
	for _, sz := range []struct{ w, h int }{
		{80, 24},
		{120, 40},
		{250, 60},
		{80, 25},
		{120, 39},
		{40, 9},
	} {
		l := remoteLayout(sz.w, sz.h)
		wantHalf := (sz.h - 5) / 2
		if l.upRows != wantHalf || l.downRows != wantHalf {
			t.Fatalf("%dx%d: got upRows=%d downRows=%d, want %d", sz.w, sz.h, l.upRows, l.downRows, wantHalf)
		}
		if l.header != 0 || l.footer != 1 || l.upPeak != 2 || l.upY != 3 {
			t.Fatalf("%dx%d: unexpected header/up layout %+v", sz.w, sz.h, l)
		}
		if l.center != l.upY+wantHalf || l.downY != l.center+1 || l.downPeak != l.downY+wantHalf {
			t.Fatalf("%dx%d: unexpected center/down layout %+v", sz.w, sz.h, l)
		}
		wantDownPeak := sz.h - 1 - (sz.h-5)%2
		if l.downPeak != wantDownPeak {
			t.Fatalf("%dx%d: downPeak=%d, want %d", sz.w, sz.h, l.downPeak, wantDownPeak)
		}

		c := remoteTestChart(remoteSpike(60, 30, 1<<20, 0), remoteSpike(60, 30, 1<<20, 0))
		b := tui.New(sz.w, sz.h)
		drawRemoteChart(b, c)
		rows := brailleRows(b)
		if len(rows) == 0 {
			t.Fatalf("%dx%d: nothing drawn", sz.w, sz.h)
		}
		if rows[0] != l.upY {
			t.Fatalf("%dx%d: first plot row with dots is %d, want %d", sz.w, sz.h, rows[0], l.upY)
		}
		lastPlotRow := l.downY + l.downRows - 1
		if rows[len(rows)-1] != lastPlotRow {
			t.Fatalf("%dx%d: last plot row with dots is %d, want %d", sz.w, sz.h, rows[len(rows)-1], lastPlotRow)
		}
		lines := strings.Split(b.String(), "\n")
		if !strings.Contains(lines[0], "▲") || !strings.Contains(lines[1], "▼") {
			t.Fatalf("%dx%d: both corner figures must be on the top rows", sz.w, sz.h)
		}
	}

	// Short layouts (h < 9) keep the compact arrangement with no peak labels.
	for _, h := range []int{3, 4, 5, 8} {
		l := remoteLayout(40, h)
		if l.header != 0 || l.footer != h-1 || l.upPeak != -1 || l.downPeak != -1 {
			t.Fatalf("h=%d: unexpected compact layout %+v", h, l)
		}
		if l.upRows+l.downRows != h-3 {
			t.Fatalf("h=%d: upRows=%d downRows=%d, want sum %d", h, l.upRows, l.downRows, h-3)
		}
	}
}

// The baseline is a dot row of the braille canvas, so an idle chart is one
// thin line of dots and a curve touches it.
func TestRemoteBaselineIsInTheCanvas(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 0), remoteFlat(60, 0))
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	rows := brailleRows(b)
	if len(rows) != 1 {
		t.Fatalf("an idle chart must be one row of dots, got rows %v:\n%s", rows, b.String())
	}
	for x := 0; x < b.Width(); x++ {
		cell := b.Cell(x, rows[0])
		if cell.Rune <= 0x2800 || cell.Style != c.Dim {
			t.Fatalf("the baseline must cross the width in the dim style, cell %d is %q:\n%s", x, cell.Rune, b.String())
		}
	}
	if strings.ContainsRune(b.String(), '─') {
		t.Fatalf("the braille chart must not draw a box line for the baseline:\n%s", b.String())
	}

	// A burst joins the baseline: the cells of the baseline row under it
	// carry the curve color.
	c = remoteTestChart(remoteSpike(60, 30, 1<<20, 0), nil)
	b = tui.New(60, 15)
	drawRemoteChart(b, c)
	touched := 0
	for x := 0; x < b.Width(); x++ {
		if b.Cell(x, rows[0]).Style == remoteGreen {
			touched++
		}
	}
	if touched == 0 {
		t.Fatalf("the burst does not touch the baseline:\n%s", b.String())
	}
}

// The chart draws lines only, as clean one dot curves: no hatch or dither under
// a curve, which reads as static in a terminal.
func TestRemoteDrawsLinesOnly(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 1<<20), remoteSpike(60, 30, 1<<20, 0))
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			if cell := b.Cell(x, y); cell.Rune > 0x2800 && cell.Rune <= 0x28ff && cell.Style.Attrs&tui.AttrDim != 0 {
				t.Fatalf("cell %d,%d is a dim fill cell:\n%s", x, y, b.String())
			}
		}
	}
}

func TestRemoteTinyRatesLieOnTheBaseline(t *testing.T) {
	// A 1 MiB/s burst sets the scale, and the rest of the window carries 0.7
	// of one dot of it.
	l := remoteLayout(60, 15)
	dots := float64(l.upRows*4 + 1)
	// The burst is a few seconds wide, so the smoothing keeps its height.
	vals := remoteSpike(60, 30, 1<<20, 0.7/dots*(1<<20))
	c := remoteTestChart(vals, nil)
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	for x := 0; x < 10; x++ {
		for y := l.upY; y < l.center; y++ {
			if r := b.Cell(x, y).Rune; r > 0x2800 && r <= 0x28ff {
				t.Fatalf("a tiny rate draws above the baseline at column %d row %d:\n%s", x, y, b.String())
			}
		}
		if cell := b.Cell(x, l.center); cell.Style != c.Dim || cell.Rune != 0x2812 {
			t.Fatalf("column %d of the baseline row is %q, want the bare baseline:\n%s", x, cell.Rune, b.String())
		}
	}
}
