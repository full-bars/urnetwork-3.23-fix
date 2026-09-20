package tui

import (
	"strings"
	"testing"
)

func TestDrawBox(t *testing.T) {
	b := New(14, 4)
	inner := DrawBox(b, "Now", BorderSingle, Style{}, Style{}, false)
	want := "┌ Now ───────┐\n│            │\n│            │\n└────────────┘"
	if got := b.String(); got != want {
		t.Fatalf("box:\n%s\nwant\n%s", got, want)
	}
	if inner.Width() != 12 || inner.Height() != 2 {
		t.Fatalf("inner = %dx%d, want 12x2", inner.Width(), inner.Height())
	}
	inner.Put("hello", 0, 0, Style{})
	if b.Cell(1, 1).Rune != 'h' {
		t.Fatal("inner view is not offset into the frame")
	}
	// Overlong content stays inside the frame.
	inner.Put("this is far too long for the box", 0, 1, Style{})
	if b.Cell(13, 2).Rune != '│' {
		t.Fatal("content overran the right border")
	}
}

func TestDrawBoxRoundedAndASCII(t *testing.T) {
	b := New(6, 3)
	DrawBox(b, "", BorderRounded, Style{}, Style{}, false)
	if got := b.String(); got != "╭────╮\n│    │\n╰────╯" {
		t.Fatalf("rounded:\n%s", got)
	}
	b = New(6, 3)
	DrawBox(b, "ab", BorderASCII, Style{}, Style{}, true)
	if got := b.String(); got != "+ ab +\n|    |\n+----+" {
		t.Fatalf("ascii:\n%s", got)
	}
}

func TestDrawBoxTitleTruncation(t *testing.T) {
	b := New(10, 3)
	DrawBox(b, "Throughput and more", BorderSingle, Style{}, Style{}, false)
	first := strings.SplitN(b.String(), "\n", 2)[0]
	if first != "┌ Throu… ┐" {
		t.Fatalf("top border = %q", first)
	}
	// A title with wide runes is cut on a rune boundary and keeps the corner.
	b = New(9, 3)
	DrawBox(b, "漢字漢字漢字", BorderSingle, Style{}, Style{}, false)
	if b.Cell(8, 0).Rune != '┐' {
		t.Fatalf("corner overwritten: %q", b.Cell(8, 0).Rune)
	}
	// Too narrow for any title: frame only.
	b = New(4, 3)
	DrawBox(b, "Title", BorderSingle, Style{}, Style{}, false)
	if got := b.String(); got != "┌──┐\n│  │\n└──┘" {
		t.Fatalf("narrow box:\n%s", got)
	}
}

func TestDrawBoxDegenerate(t *testing.T) {
	for _, dim := range [][2]int{{0, 0}, {1, 5}, {5, 1}, {2, 2}} {
		b := New(dim[0], dim[1])
		inner := DrawBox(b, "x", BorderSingle, Style{}, Style{}, false)
		if inner.Width() != max(dim[0]-2, 0) && dim[0] >= 2 && dim[1] >= 2 {
			t.Errorf("%v: inner %dx%d", dim, inner.Width(), inner.Height())
		}
		inner.Put("x", 0, 0, Style{}) // must not panic or escape
	}
	b := New(1, 1)
	DrawBox(b, "x", BorderSingle, Style{}, Style{}, false)
	if b.String() != "" {
		t.Fatalf("1x1 should draw nothing, got %q", b.String())
	}
	b = New(2, 2)
	inner := DrawBox(b, "", BorderSingle, Style{}, Style{}, false)
	if inner.Width() != 0 || b.String() != "┌┐\n└┘" {
		t.Fatalf("2x2: %q inner %dx%d", b.String(), inner.Width(), inner.Height())
	}
}
