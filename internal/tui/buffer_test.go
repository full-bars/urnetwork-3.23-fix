package tui

import (
	"strings"
	"testing"
)

func TestNewBlank(t *testing.T) {
	b := New(3, 2)
	if got := b.String(); got != "\n" {
		t.Fatalf("blank buffer String() = %q, want a single newline between empty rows", got)
	}
	if b.Width() != 3 || b.Height() != 2 {
		t.Fatalf("size = %dx%d", b.Width(), b.Height())
	}
	z := New(-1, 5)
	if z.Width() != 0 || z.Height() != 5 {
		t.Fatalf("negative width should clamp to 0, got %dx%d", z.Width(), z.Height())
	}
	z.Put("x", 0, 0, Style{}) // must not panic
}

func TestPutClipsAtEdges(t *testing.T) {
	cases := []struct {
		name string
		text string
		x, y int
		want string
		next int
	}{
		{"fits", "abc", 0, 0, "abc  ", 3},
		{"clips right", "abcdefgh", 2, 0, "  abc", 10},
		{"clips left", "abcdef", -2, 0, "cdef ", 4},
		{"row out of range is a no-op", "abc", 0, 5, "     ", 3},
		{"negative row", "abc", 0, -1, "     ", 3},
		{"controls dropped", "a\tb\nc", 0, 0, "abc  ", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := New(5, 1)
			next := b.Put(c.text, c.x, c.y, Style{})
			row := ""
			for x := 0; x < 5; x++ {
				row += string(b.Cell(x, 0).Rune)
			}
			if row != c.want {
				t.Fatalf("row = %q, want %q", row, c.want)
			}
			if next != c.next {
				t.Fatalf("next x = %d, want %d", next, c.next)
			}
		})
	}
}

func TestPutWideRunes(t *testing.T) {
	b := New(6, 1)
	next := b.Put("a漢b", 0, 0, Style{})
	if next != 4 {
		t.Fatalf("next = %d, want 4 (wide rune advances two)", next)
	}
	if !b.Cell(2, 0).Continuation() || b.Cell(1, 0).Rune != '漢' {
		t.Fatalf("wide rune should sit at 1 with a continuation at 2: %+v %+v", b.Cell(1, 0), b.Cell(2, 0))
	}
	if got := b.String(); got != "a漢b" {
		t.Fatalf("String() = %q", got)
	}
}

func TestPutWideRuneNeverHalfDrawn(t *testing.T) {
	// Right edge: a wide rune with one cell left becomes a blank.
	b := New(3, 1)
	b.Put("ab漢", 0, 0, Style{})
	if got := b.String(); got != "ab" {
		t.Fatalf("right clip String() = %q, want %q", got, "ab")
	}
	if b.Cell(2, 0).Rune != ' ' {
		t.Fatalf("clipped wide rune left %q, want a blank", b.Cell(2, 0).Rune)
	}
	// Left edge: only the trailing half is visible, so it is blank.
	b = New(3, 1)
	b.Put("漢b", -1, 0, Style{})
	if got := b.String(); got != " b" {
		t.Fatalf("left clip String() = %q (cell 0 should be blank, b at 1)", got)
	}
	if b.Cell(0, 0).Rune != ' ' || b.Cell(0, 0).Continuation() {
		t.Fatalf("cell 0 = %+v, want a blank", b.Cell(0, 0))
	}
}

func TestOverwriteRepairsWideRune(t *testing.T) {
	b := New(4, 1)
	b.Put("漢字", 0, 0, Style{})
	// Overwrite the continuation cell: the head must not survive as a half glyph.
	b.Put("x", 1, 0, Style{})
	if b.Cell(0, 0).Rune != ' ' || b.Cell(1, 0).Rune != 'x' {
		t.Fatalf("after overwriting the tail: %q %q", b.Cell(0, 0).Rune, b.Cell(1, 0).Rune)
	}
	// Overwrite the head: the orphaned continuation must go too.
	b = New(4, 1)
	b.Put("漢字", 0, 0, Style{})
	b.Put("y", 2, 0, Style{})
	if b.Cell(3, 0).Continuation() || b.Cell(3, 0).Rune != ' ' {
		t.Fatalf("orphaned continuation left behind: %+v", b.Cell(3, 0))
	}
	// A wide rune written over two narrow ones, and over the middle of two wides.
	b = New(4, 1)
	b.Put("漢字", 0, 0, Style{})
	b.Put("界", 1, 0, Style{})
	if got := b.String(); got != " 界" {
		t.Fatalf("String() = %q, want blanks around the new wide rune", got)
	}
	for x := 0; x < 4; x++ {
		c := b.Cell(x, 0)
		if c.Continuation() && (x == 0 || RuneWidth(b.Cell(x-1, 0).Rune) != 2) {
			t.Fatalf("continuation at %d without a wide head", x)
		}
	}
}

func TestPutCombining(t *testing.T) {
	b := New(4, 1)
	next := b.Put("éx", 0, 0, Style{})
	if next != 2 {
		t.Fatalf("combining mark must not advance: next = %d", next)
	}
	if b.Cell(0, 0).Comb != "́" {
		t.Fatalf("mark not attached: %+v", b.Cell(0, 0))
	}
	if got := b.String(); got != "éx" {
		t.Fatalf("String() = %q", got)
	}
	// A leading mark with nothing to attach to is dropped.
	b = New(2, 1)
	b.Put("́a", 0, 0, Style{})
	if got := b.String(); got != "a" {
		t.Fatalf("String() = %q", got)
	}
	// A mark on a clipped rune is dropped, not attached to a neighbour.
	b = New(2, 1)
	b.Put("ab́", 1, 0, Style{})
	b.Put("́", 0, 0, Style{}) // new Put, nothing to attach
	if b.Cell(0, 0).Comb != "" {
		t.Fatalf("mark leaked onto cell 0: %+v", b.Cell(0, 0))
	}
}

func TestFillAndSub(t *testing.T) {
	b := New(6, 3)
	sub := b.Sub(Rect{X: 2, Y: 1, W: 3, H: 2})
	if sub.Width() != 3 || sub.Height() != 2 {
		t.Fatalf("sub size %dx%d, want 3x2 (clipped to parent)", sub.Width(), sub.Height())
	}
	sub.Fill(sub.Rect(), '#', Style{})
	sub.Put("toolong", 0, 0, Style{})
	want := "\n  too\n  ###"
	if got := b.String(); got != want {
		t.Fatalf("parent after drawing in sub:\n%q\nwant\n%q", got, want)
	}
	// Writes outside the view do not reach the parent.
	sub.Set(-1, 0, Cell{Rune: 'X'})
	sub.Set(3, 0, Cell{Rune: 'X'})
	sub.Set(0, 2, Cell{Rune: 'X'})
	if got := b.String(); got != want {
		t.Fatalf("out-of-view Set leaked:\n%q", got)
	}
	// Fill clips rather than panicking.
	sub.Fill(Rect{X: -5, Y: -5, W: 100, H: 100}, '.', Style{})
	if got := b.String(); got != "\n  ...\n  ..." {
		t.Fatalf("clipped fill:\n%q", got)
	}
	// A view outside the parent is empty and safe.
	out := b.Sub(Rect{X: 10, Y: 10, W: 2, H: 2})
	if out.Width() != 0 || out.Height() != 0 {
		t.Fatalf("outside sub = %dx%d", out.Width(), out.Height())
	}
	out.Put("x", 0, 0, Style{})
	// Nested views compose their offsets.
	inner := sub.Sub(Rect{X: 1, Y: 1, W: 5, H: 5})
	inner.Put("Z", 0, 0, Style{})
	if b.Cell(3, 2).Rune != 'Z' {
		t.Fatalf("nested view offset wrong: %q", b.Cell(3, 2).Rune)
	}
}

func TestFillRejectsWideRune(t *testing.T) {
	b := New(2, 1)
	b.Fill(b.Rect(), '漢', Style{})
	if got := b.String(); got != "" {
		t.Fatalf("wide fill rune should degrade to spaces, got %q", got)
	}
}

func TestSubRepairsWideRuneAcrossEdge(t *testing.T) {
	// The parent draws a wide rune that straddles the child's left edge; the
	// child overwriting its first column must not leave a half glyph behind.
	b := New(4, 1)
	b.Put("漢", 0, 0, Style{})
	child := b.Sub(Rect{X: 1, Y: 0, W: 3, H: 1})
	child.Put("x", 0, 0, Style{})
	if b.Cell(0, 0).Rune != ' ' || b.Cell(1, 0).Rune != 'x' {
		t.Fatalf("straddling wide rune not repaired: %+v %+v", b.Cell(0, 0), b.Cell(1, 0))
	}
}

func TestResize(t *testing.T) {
	b := New(4, 2)
	b.Put("abcd", 0, 0, Style{})
	b.Put("wxyz", 0, 1, Style{})
	b.Resize(6, 3)
	if got := b.String(); got != "abcd\nwxyz\n" {
		t.Fatalf("grow kept content wrong: %q", got)
	}
	b.Resize(2, 1)
	if got := b.String(); got != "ab" {
		t.Fatalf("shrink: %q", got)
	}
	// A wide rune cut by the new edge is blanked.
	b = New(4, 1)
	b.Put("a漢b", 0, 0, Style{})
	b.Resize(2, 1)
	if got := b.String(); got != "a" || b.Cell(1, 0).Rune != ' ' {
		t.Fatalf("cut wide rune: %q %+v", got, b.Cell(1, 0))
	}
	b.Resize(0, 0)
	if b.String() != "" {
		t.Fatal("zero-size buffer should render empty")
	}
	b.Resize(3, 1) // and back up from zero
	b.Put("ok", 0, 0, Style{})
	if b.String() != "ok" {
		t.Fatalf("regrow from zero: %q", b.String())
	}
}

func TestResizeOnViewPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Resize on a Sub view should panic")
		}
	}()
	New(2, 2).Sub(Rect{W: 1, H: 1}).Resize(3, 3)
}

func TestClone(t *testing.T) {
	b := New(5, 3)
	b.Put("hello", 0, 1, Style{})
	c := b.Sub(Rect{X: 1, Y: 1, W: 3, H: 1}).Clone()
	if c.String() != "ell" || c.Width() != 3 || c.Height() != 1 {
		t.Fatalf("clone of a view = %q %dx%d", c.String(), c.Width(), c.Height())
	}
	b.Put("J", 1, 1, Style{})
	if c.String() != "ell" {
		t.Fatal("clone shares storage with its source")
	}
}

func TestStringTrimsTrailingSpacesAndSkipsContinuations(t *testing.T) {
	b := New(6, 2)
	b.Put("漢 ", 0, 0, Style{})
	b.Put("ab  ", 0, 1, Style{})
	if got := b.String(); got != "漢\nab" {
		t.Fatalf("String() = %q", got)
	}
	if strings.ContainsRune(b.String(), 0) {
		t.Fatal("continuation cell leaked into text")
	}
}

func TestStyleIsStoredPerCell(t *testing.T) {
	st := Style{FG: Ansi(2), BG: RGB(1, 2, 3)}.With(AttrBold)
	b := New(2, 1)
	b.Put("a", 0, 0, st)
	if got := b.Cell(0, 0).Style; got != st {
		t.Fatalf("style = %+v", got)
	}
	r, g, bl := st.BG.Components()
	if r != 1 || g != 2 || bl != 3 {
		t.Fatalf("components = %d %d %d", r, g, bl)
	}
	if Ansi(17).Value != 1 || Default().Kind != ColorDefault || Xterm(200).Kind != Color256 {
		t.Fatal("color constructors")
	}
	if st.Fg(Default()).FG != Default() || st.Bg(Default()).BG != Default() {
		t.Fatal("Fg/Bg setters")
	}
}
