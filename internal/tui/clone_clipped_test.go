package tui

import "testing"

func TestCloneClippedWideRuneEdges(t *testing.T) {
	b := New(4, 2)
	b.Set(1, 0, Cell{Rune: '界'})
	left := b.Sub(Rect{X: 2, Y: 0, W: 2, H: 1}).Clone()
	if got := left.Cell(0, 0); got.Rune != ' ' || got.Continuation() {
		t.Fatalf("left clipped continuation = %+v", got)
	}
	right := b.Sub(Rect{X: 0, Y: 0, W: 2, H: 1}).Clone()
	if got := right.Cell(1, 0); got.Rune != ' ' || got.Continuation() {
		t.Fatalf("right clipped head = %+v", got)
	}
	intact := b.Sub(Rect{X: 1, Y: 0, W: 2, H: 1}).Clone()
	if intact.Cell(0, 0).Rune != '界' || !intact.Cell(1, 0).Continuation() {
		t.Fatal("intact wide rune was damaged")
	}
	// A view ending at the backing row's edge must not inspect the next row.
	b.Set(0, 1, Cell{Rune: '界'})
	last := b.Sub(Rect{X: 2, Y: 1, W: 2, H: 1}).Clone()
	if last.Cell(1, 0).Rune != ' ' {
		t.Fatal("last column changed")
	}
}
