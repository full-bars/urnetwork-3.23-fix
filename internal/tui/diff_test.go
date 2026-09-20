package tui

import (
	"reflect"
	"testing"
)

func runText(r Run) string {
	s := ""
	for _, c := range r.Cells {
		if !c.Continuation() {
			s += string(c.Rune)
		}
	}
	return s
}

func TestDiffIdenticalIsEmpty(t *testing.T) {
	a := New(5, 3)
	a.Put("hello", 0, 1, Style{})
	if runs := Diff(a.Clone(), a); len(runs) != 0 {
		t.Fatalf("identical frames gave %d runs", len(runs))
	}
}

func TestDiffMinimalRuns(t *testing.T) {
	prev := New(8, 2)
	prev.Put("abcdefgh", 0, 0, Style{})
	cur := prev.Clone()
	cur.Put("XY", 1, 0, Style{})
	cur.Put("Z", 5, 0, Style{})
	cur.Put("!", 7, 1, Style{})
	runs := Diff(prev, cur)
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3: %+v", len(runs), runs)
	}
	want := []struct {
		x, y int
		text string
	}{{1, 0, "XY"}, {5, 0, "Z"}, {7, 1, "!"}}
	for i, w := range want {
		r := runs[i]
		if r.X != w.x || r.Y != w.y || runText(r) != w.text {
			t.Errorf("run %d = (%d,%d,%q), want (%d,%d,%q)", i, r.X, r.Y, runText(r), w.x, w.y, w.text)
		}
	}
}

func TestDiffStyleOnlyChange(t *testing.T) {
	prev := New(3, 1)
	prev.Put("abc", 0, 0, Style{})
	cur := prev.Clone()
	cur.Put("b", 1, 0, Style{FG: Ansi(1)})
	runs := Diff(prev, cur)
	if len(runs) != 1 || runs[0].X != 1 || len(runs[0].Cells) != 1 {
		t.Fatalf("style-only change should yield one 1-cell run: %+v", runs)
	}
}

func TestDiffKeepsWideRuneWhole(t *testing.T) {
	prev := New(5, 1)
	prev.Put("ab漢c", 0, 0, Style{})
	// Only the head changes to a different wide rune: the run must carry both cells.
	cur := prev.Clone()
	cur.Put("字", 2, 0, Style{})
	runs := Diff(prev, cur)
	if len(runs) != 1 || runs[0].X != 2 || len(runs[0].Cells) != 2 || !runs[0].Cells[1].Continuation() {
		t.Fatalf("wide change: %+v", runs)
	}
	// A narrow rune written over a wide one changes head and tail.
	cur = prev.Clone()
	cur.Put("x", 3, 0, Style{}) // over the continuation cell
	runs = Diff(prev, cur)
	if len(runs) != 1 || runs[0].X != 2 || len(runs[0].Cells) != 2 {
		t.Fatalf("narrow over wide tail should resend both cells: %+v", runs)
	}
	for _, r := range runs {
		if r.Cells[0].Continuation() {
			t.Fatalf("run starts on a continuation cell: %+v", r)
		}
	}
}

func TestDiffResizeResendsEverything(t *testing.T) {
	prev := New(3, 2)
	cur := New(4, 2)
	cur.Put("hi", 0, 0, Style{})
	runs := Diff(prev, cur)
	if len(runs) != 2 || len(runs[0].Cells) != 4 || runs[1].Y != 1 {
		t.Fatalf("size change should return every row: %+v", runs)
	}
	if got := Diff(nil, cur); len(got) != 2 {
		t.Fatalf("nil prev should return every row, got %d runs", len(got))
	}
	if got := Diff(prev, New(0, 0)); got != nil {
		t.Fatalf("empty cur: %+v", got)
	}
}

func TestDiffRunsOwnTheirCells(t *testing.T) {
	prev := New(3, 1)
	cur := New(3, 1)
	cur.Put("abc", 0, 0, Style{})
	runs := Diff(prev, cur)
	cur.Put("zzz", 0, 0, Style{})
	if runText(runs[0]) != "abc" {
		t.Fatalf("run aliases the live buffer: %q", runText(runs[0]))
	}
}

// Applying the diff to the previous frame must reproduce the current one, for
// any pair of frames. This is the property a diffing writer depends on.
func TestDiffApplyReproducesCurrent(t *testing.T) {
	texts := []string{"", "hello", "漢字漢", "a漢b字c", "  x  ", "éé"}
	for _, pt := range texts {
		for _, ct := range texts {
			prev, cur := New(7, 1), New(7, 1)
			prev.Put(pt, 0, 0, Style{})
			cur.Put(ct, 0, 0, Style{FG: Ansi(3)})
			got := prev.Clone()
			for _, r := range Diff(prev, cur) {
				for i, c := range r.Cells {
					got.cells[got.index(r.X+i, r.Y)] = c
				}
			}
			if !reflect.DeepEqual(got.cells, cur.cells) {
				t.Fatalf("apply(diff(%q -> %q)) != cur", pt, ct)
			}
		}
	}
}
