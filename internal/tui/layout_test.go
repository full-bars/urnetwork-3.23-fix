package tui

import (
	"reflect"
	"testing"
)

func TestSplitRowsFixedAndFlex(t *testing.T) {
	got := SplitRows(Rect{X: 2, Y: 1, W: 10, H: 20}, Fixed(1), Flex(2), Flex(1), Fixed(1))
	want := []Rect{
		{X: 2, Y: 1, W: 10, H: 1},
		{X: 2, Y: 2, W: 10, H: 12},
		{X: 2, Y: 14, W: 10, H: 6},
		{X: 2, Y: 20, W: 10, H: 1},
	}
	// 18 left over shared 2:1 is 12 and 6.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSplitColsFixedAndFlex(t *testing.T) {
	got := SplitCols(Rect{W: 100, H: 5}, Flex(1), Fixed(30))
	want := []Rect{{X: 0, Y: 0, W: 70, H: 5}, {X: 70, Y: 0, W: 30, H: 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSplitSizesAlwaysSumExactly(t *testing.T) {
	// Awkward divisions must not lose or invent cells.
	for total := 0; total < 60; total++ {
		for _, tracks := range [][]Track{
			{Flex(1), Flex(1), Flex(1)},
			{Flex(3), Flex(2), Flex(7)},
			{Fixed(4), Flex(1), Flex(2), Fixed(3)},
		} {
			sum := 0
			for _, s := range split(total, tracks) {
				if s < 0 {
					t.Fatalf("negative size at total %d", total)
				}
				sum += s
			}
			if sum != total {
				t.Fatalf("total %d tracks %+v: sizes sum to %d", total, tracks, sum)
			}
		}
	}
}

func TestSplitStarvesFlexBeforeFixed(t *testing.T) {
	// Only 3 cells: header and footer keep theirs, the flexible body gets 1.
	got := split(3, []Track{Fixed(1), Flex(1), Fixed(1)})
	if !reflect.DeepEqual(got, []int{1, 1, 1}) {
		t.Fatalf("got %v", got)
	}
	// Too small even for the fixed ones: served in order, never negative.
	got = split(1, []Track{Fixed(1), Flex(1), Fixed(1)})
	if !reflect.DeepEqual(got, []int{1, 0, 0}) {
		t.Fatalf("got %v", got)
	}
}

func TestSplitEdgeCases(t *testing.T) {
	if got := split(10, []Track{Fixed(2), Fixed(3)}); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("no flex leaves space unused: %v", got)
	}
	if got := split(10, []Track{Flex(0), Flex(1)}); !reflect.DeepEqual(got, []int{0, 10}) {
		t.Fatalf("zero weight takes nothing: %v", got)
	}
	if got := split(-5, []Track{Flex(1)}); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("negative total: %v", got)
	}
	if got := Fixed(-3); got.fixed != 0 {
		t.Fatalf("negative fixed: %+v", got)
	}
	if got := SplitRows(Rect{W: -1, H: 4}, Flex(1)); got[0].W != 0 {
		t.Fatalf("negative width: %+v", got)
	}
}

func TestMinSize(t *testing.T) {
	m := MinSize{W: 72, H: 20}
	cases := []struct {
		w, h int
		want bool
	}{{120, 40, true}, {80, 24, true}, {72, 20, true}, {71, 20, false}, {72, 19, false}, {40, 10, false}, {0, 0, false}}
	for _, c := range cases {
		if got := m.Fits(c.w, c.h); got != c.want {
			t.Errorf("Fits(%d,%d) = %v, want %v", c.w, c.h, got, c.want)
		}
	}
	if !m.FitsRect(Rect{W: 100, H: 30}) || m.FitsRect(Rect{W: 10, H: 10}) {
		t.Fatal("FitsRect")
	}
}

func TestRect(t *testing.T) {
	a := Rect{X: 0, Y: 0, W: 10, H: 10}
	if got := a.Intersect(Rect{X: 5, Y: 5, W: 10, H: 10}); got != (Rect{X: 5, Y: 5, W: 5, H: 5}) {
		t.Fatalf("intersect: %+v", got)
	}
	if got := a.Intersect(Rect{X: 20, Y: 0, W: 5, H: 5}); !got.Empty() {
		t.Fatalf("disjoint: %+v", got)
	}
	if got := a.Inset(1); got != (Rect{X: 1, Y: 1, W: 8, H: 8}) {
		t.Fatalf("inset: %+v", got)
	}
	if got := (Rect{W: 2, H: 2}).Inset(1); !got.Empty() {
		t.Fatalf("inset to nothing: %+v", got)
	}
}
