package tui

// Run is a horizontal stretch of changed cells starting at (X, Y). It owns
// its Cells, so a backend can hold runs after the buffers are drawn into
// again.
type Run struct {
	X, Y  int
	Cells []Cell
}

// Diff returns the runs of cells that differ between prev and cur, in reading
// order, for a writer that only sends what changed. Only exactly-changed
// cells are reported; whether to merge two runs separated by a short unchanged
// gap (cheaper than a cursor move) is the writer's call, since that depends on
// its escape encoding.
//
// A wide rune and its continuation cell always travel together, so a run never
// begins or ends in the middle of one. If the sizes differ, or prev is nil,
// every row of cur is returned in full: a resize invalidates the screen.
func Diff(prev, cur *Buffer) []Run {
	if cur == nil || cur.w == 0 || cur.h == 0 {
		return nil
	}
	full := prev == nil || prev.w != cur.w || prev.h != cur.h
	var runs []Run
	changed := make([]bool, cur.w)
	for y := 0; y < cur.h; y++ {
		any := false
		for x := 0; x < cur.w; x++ {
			changed[x] = full || prev.cells[prev.index(x, y)] != cur.cells[cur.index(x, y)]
			any = any || changed[x]
		}
		if !any {
			continue
		}
		// Pull each half of a wide rune in with the other half.
		for x := 0; x < cur.w; x++ {
			if !changed[x] {
				continue
			}
			c := cur.cells[cur.index(x, y)]
			if c.Continuation() && x > 0 {
				changed[x-1] = true
			}
			if x+1 < cur.w && cur.cells[cur.index(x+1, y)].Continuation() {
				changed[x+1] = true
			}
		}
		for x := 0; x < cur.w; {
			if !changed[x] {
				x++
				continue
			}
			start := x
			for x < cur.w && changed[x] {
				x++
			}
			cells := make([]Cell, x-start)
			copy(cells, cur.cells[cur.index(start, y):cur.index(x, y)])
			runs = append(runs, Run{X: start, Y: y, Cells: cells})
		}
	}
	return runs
}
