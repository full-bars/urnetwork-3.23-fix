package tui

import (
	"math"
	"testing"
)

func barRow(bar Bar, w int, ascii bool) string {
	b := New(w, 1)
	DrawBar(b, bar, ascii)
	return b.String()
}

func TestBarFill(t *testing.T) {
	cases := []struct {
		name string
		bar  Bar
		w    int
		want string
	}{
		{"empty", Bar{Frac: 0}, 5, "░░░░░"},
		{"full", Bar{Frac: 1}, 5, "█████"},
		{"half of ten", Bar{Frac: 0.5}, 10, "█████░░░░░"},
		{"clamped high", Bar{Frac: 7}, 4, "████"},
		{"clamped low", Bar{Frac: -1}, 4, "░░░░"},
		{"nan is empty", Bar{Frac: math.NaN()}, 4, "░░░░"},
		{"inf is full", Bar{Frac: math.Inf(1)}, 4, "████"},
		{"sub-cell partial", Bar{Frac: 0.3125}, 4, "█▎░░"}, // 1.25 cells
		{"tiny nonzero rounds to nothing", Bar{Frac: 0.001}, 4, "░░░░"},
		{"eighth", Bar{Frac: 1.0 / 32}, 4, "▏░░░"},
		{"with label and value", Bar{Label: "heap", Value: "1.8 GiB", Frac: 0.5}, 20, "heap ███▌░░░ 1.8 GiB"},
		{"label padded to fixed width", Bar{Label: "fds", LabelWidth: 6, Value: "9", Frac: 1}, 16, "fds    ███████ 9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := barRow(c.bar, c.w, false)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if StringWidth(got) > c.w {
				t.Fatalf("drew %d cells into %d", StringWidth(got), c.w)
			}
		})
	}
}

func TestBarASCII(t *testing.T) {
	if got := barRow(Bar{Frac: 0.5}, 6, true); got != "###---" {
		t.Fatalf("ascii bar = %q", got)
	}
	if got := barRow(Bar{Frac: 0.34}, 6, true); got != "##----" {
		t.Fatalf("ascii rounds to whole cells: %q", got)
	}
}

func TestBarSqueezed(t *testing.T) {
	// No room for a gauge beside the value: the value goes, the gauge stays.
	if got := barRow(Bar{Label: "heap", Value: "1.8 GiB", Frac: 0.5}, 9, false); got != "heap ██░░" {
		t.Fatalf("value should be dropped first: %q", got)
	}
	// No room for a gauge at all: label only, truncated to fit.
	if got := barRow(Bar{Label: "heap", Value: "x", Frac: 1}, 4, false); got != "heap" {
		t.Fatalf("label only: %q", got)
	}
	if got := barRow(Bar{Label: "heapheap", Frac: 1}, 5, false); got != "heap…" {
		t.Fatalf("truncated label: %q", got)
	}
	// Degenerate buffers never panic.
	DrawBar(New(0, 1), Bar{Frac: 1}, false)
	DrawBar(New(3, 0), Bar{Frac: 1}, false)
}

func TestBarStyles(t *testing.T) {
	fill, empty := Style{FG: Ansi(2)}, Style{FG: Ansi(8)}
	b := New(4, 1)
	DrawBar(b, Bar{Frac: 0.5, Fill: fill, Empty: empty}, false)
	if b.Cell(0, 0).Style != fill || b.Cell(3, 0).Style != empty {
		t.Fatal("fill and empty styles not applied to their cells")
	}
}

func TestGaugeCellsNeverExceedsWidth(t *testing.T) {
	for w := 1; w <= 12; w++ {
		for i := 0; i <= 200; i++ {
			frac := float64(i) / 200
			full, part := gaugeCells(frac, w, false)
			if full > w || (full == w && part != 0) {
				t.Fatalf("w=%d frac=%v full=%d part=%d", w, frac, full, part)
			}
		}
	}
}
