package urnettools

import (
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// remoteDotGrid decodes the braille cells of rows [y0, y0+rows) of b into a
// dot grid of curve dots: dots of cells that are neither dim (the fill) nor
// in the baseline style.
func remoteDotGrid(b *tui.Buffer, y0, rows int, base tui.Style) [][]bool {
	bits := [8][2]int{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {1, 1}, {1, 2}, {0, 3}, {1, 3}}
	grid := make([][]bool, rows*4)
	for i := range grid {
		grid[i] = make([]bool, b.Width()*2)
	}
	for r := 0; r < rows; r++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y0+r)
			if c.Rune <= 0x2800 || c.Rune > 0x28ff || c.Style.Attrs&tui.AttrDim != 0 || c.Style == base {
				continue
			}
			m := int(c.Rune - 0x2800)
			for k := 0; k < 8; k++ {
				if m&(1<<k) != 0 {
					grid[r*4+bits[k][1]][x*2+bits[k][0]] = true
				}
			}
		}
	}
	return grid
}

// longestRun is the longest vertical run of curve dots in any dot column of
// the up half, above the baseline dot row.
func longestRun(grid [][]bool, baseRow int) int {
	best := 0
	for x := range grid[0] {
		run := 0
		for y := 0; y < baseRow; y++ {
			if grid[y][x] {
				run++
				best = max(best, run)
			} else {
				run = 0
			}
		}
	}
	return best
}

// A one-second burst is a narrow peak through its sample, like the app's curve.
// It is two samples wide at the foot and rounds off at the tip, so it never
// hangs as a column from a flat tip.
func TestRemoteSpikeTapers(t *testing.T) {
	vals := remoteFlat(60, 0)
	vals[30] = 1 << 20
	c := remoteTestChart(vals, nil)
	b := tui.New(118, 36)
	drawRemoteChart(b, c)
	l := remoteLayout(118, 36)
	grid := remoteDotGrid(b, l.upY, l.upRows+1, c.Dim)
	maxH := l.upRows*4 + 1
	if run := longestRun(grid, maxH); run*2 > maxH {
		t.Fatalf("the burst hangs as a run of %d dots of a %d dot half:\n%s", run, maxH, b.String())
	}
	// The tip still reaches the top rows of the half: the peak is not
	// flattened away.
	top := -1
	for y := 0; y < maxH && top < 0; y++ {
		for _, on := range grid[y] {
			if on {
				top = y
				break
			}
		}
	}
	if top < 0 || top > maxH/4 {
		t.Fatalf("the burst tip is at dot row %d of %d:\n%s", top, maxH, b.String())
	}
}

// A history that fills the window starts at the left edge at its own height.
// It does not rise from the baseline in the first dot column, which would draw
// a wall at the edge of the plot.
func TestRemoteFullHistoryHasNoWallAtTheLeftEdge(t *testing.T) {
	c := remoteTestChart(remoteFlat(120, 1<<20), nil)
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	l := remoteLayout(60, 15)
	grid := remoteDotGrid(b, l.upY, l.upRows+1, c.Dim)
	maxH := l.upRows*4 + 1
	run := 0
	for y := 0; y < maxH; y++ {
		if grid[y][0] {
			run++
		}
	}
	if run > 2 {
		t.Fatalf("the first dot column carries %d curve dots:\n%s", run, b.String())
	}
}
