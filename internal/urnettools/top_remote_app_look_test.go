package urnettools

import (
	"math/bits"
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

// The plot is wide and flat like the app's: each half is about a ninth of
// the width in dots, however tall the page is, and both corner figures sit
// on the top rows.
func TestRemotePlotIsFlat(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 30, 1<<20, 0), remoteSpike(60, 30, 1<<20, 0))
	b := tui.New(118, 36)
	drawRemoteChart(b, c)
	rows := brailleRows(b)
	if len(rows) == 0 {
		t.Fatal("nothing drawn")
	}
	span := rows[len(rows)-1] - rows[0] + 1
	// 236 dots wide: about 26 dots, 7 rows, per half, plus the line.
	if span > 2*7+1 {
		t.Fatalf("the plot spans %d rows of 36:\n%s", span, b.String())
	}
	lines := strings.Split(b.String(), "\n")
	if !strings.Contains(lines[0], "▲") || !strings.Contains(lines[1], "▼") {
		t.Fatalf("both corner figures must be on the top rows:\n%s", b.String())
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

// The fill under a byte curve is a sparse hatch: at most one dot in six of
// the area between the curve and the line.
func TestRemoteFillIsSparse(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 1<<20), nil)
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	fillDots, area := 0, 0
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			cell := b.Cell(x, y)
			if cell.Rune < 0x2800 || cell.Rune > 0x28ff {
				continue
			}
			if cell.Style.Attrs&tui.AttrDim != 0 {
				fillDots += bits.OnesCount32(uint32(cell.Rune - 0x2800))
				area += 8
			}
		}
	}
	if fillDots == 0 {
		t.Fatalf("a full byte curve must have a fill:\n%s", b.String())
	}
	if fillDots*6 > area {
		t.Fatalf("the fill has %d dots in %d cells of area:\n%s", fillDots, area/8, b.String())
	}
}

// A rate smaller than one dot of the scale lies on the baseline, as a quiet
// stretch does in the app, instead of a flat line one dot above it.
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
