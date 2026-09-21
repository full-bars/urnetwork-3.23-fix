package tui

import "testing"

func drawT(w, h int, tb Table, ascii bool) (string, int) {
	b := New(w, h)
	n := DrawTable(b, tb, ascii)
	return b.String(), n
}

func TestTableAlignment(t *testing.T) {
	tb := Table{
		Header: true,
		Columns: []Column{
			{Title: "proxy", Width: 8},
			{Title: "rate", Width: 6, Align: AlignRight},
			{Title: "st", Width: 4, Align: AlignCenter},
		},
		Rows: []Row{
			{Cells: []string{"alpha", "4.1", "up"}},
			{Cells: []string{"b", "12.5", "x"}},
		},
	}
	got, n := drawT(20, 3, tb, false)
	want := "proxy      rate  st\nalpha       4.1  up\nb          12.5  x"
	if got != want || n != 2 {
		t.Fatalf("n=%d got\n%s\nwant\n%s", n, got, want)
	}
}

func TestTableFlexColumnsShareLeftover(t *testing.T) {
	tb := Table{Columns: []Column{{Width: 4}, {}, {}}, Rows: []Row{{Cells: []string{"a", "bbbbbbbbbbbb", "c"}}}}
	// 20 wide, 2 gaps: 18 cells, 4 fixed, 14 split 7 and 7.
	got, _ := drawT(20, 1, tb, false)
	if want := "a    bbbbbb… c"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTableTruncationRespectsWideRunes(t *testing.T) {
	tb := Table{Columns: []Column{{Width: 5}, {Width: 3, Align: AlignRight}}, Rows: []Row{
		{Cells: []string{"漢字漢字", "abcdef"}},
		{Cells: []string{"ab漢字", "漢"}},
	}}
	got, _ := drawT(9, 2, tb, false)
	// 5 cells: "漢字" is 4, adding another would be 6, so "漢字" + ellipsis is 5.
	// Second row: "ab漢" is 4, +ellipsis 5. The wide rune in the right-aligned
	// 3-cell column takes 2 and is padded on the left.
	want := "漢字… ab…\nab漢…  漢"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	b := New(9, 2)
	DrawTable(b, tb, false)
	for y := 0; y < 2; y++ {
		for x := 0; x < 9; x++ {
			c := b.Cell(x, y)
			if c.Continuation() && (x == 0 || RuneWidth(b.Cell(x-1, y).Rune) != 2) {
				t.Fatalf("orphan continuation at (%d,%d)", x, y)
			}
		}
	}
}

func TestTableASCIIEllipsis(t *testing.T) {
	tb := Table{Columns: []Column{{Width: 6}}, Rows: []Row{{Cells: []string{"abcdefghij"}}}}
	if got, _ := drawT(6, 1, tb, true); got != "abc..." {
		t.Fatalf("got %q", got)
	}
}

func TestTableClipsRowsToHeight(t *testing.T) {
	rows := make([]Row, 10)
	for i := range rows {
		rows[i] = Row{Cells: []string{string(rune('a' + i))}}
	}
	tb := Table{Columns: []Column{{Title: "k"}}, Rows: rows, Header: true}
	got, n := drawT(3, 4, tb, false)
	if n != 3 || got != "k\na\nb\nc" {
		t.Fatalf("n=%d got %q", n, got)
	}
	if got, n = drawT(3, 1, tb, false); n != 0 || got != "k" {
		t.Fatalf("header only: n=%d got %q", n, got)
	}
	tb.Header = false
	if _, n = drawT(3, 4, tb, false); n != 4 {
		t.Fatalf("no header should give the row back: n=%d", n)
	}
}

func TestTableShortAndLongRows(t *testing.T) {
	tb := Table{Columns: []Column{{Width: 2}, {Width: 2}}, Rows: []Row{
		{Cells: []string{"a"}},
		{Cells: []string{"a", "b", "extra"}},
		{},
	}}
	got, n := drawT(5, 3, tb, false)
	if want := "a\na  b\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if n != 3 {
		t.Fatalf("n = %d", n)
	}
}

func TestTableRowStyleCoversWholeRow(t *testing.T) {
	st := Style{BG: Ansi(1)}
	tb := Table{Columns: []Column{{Width: 2}, {Width: 2}}, Rows: []Row{{Cells: []string{"a", "b"}, Style: st}}}
	b := New(5, 1)
	DrawTable(b, tb, false)
	for x := 0; x < 5; x++ {
		if b.Cell(x, 0).Style != st {
			t.Fatalf("cell %d not styled, so a highlighted row would have gaps", x)
		}
	}
}

func TestTableStarvedColumns(t *testing.T) {
	// Fixed columns win over flexible ones when the width runs out.
	tb := Table{Columns: []Column{{Width: 4}, {}, {Width: 4}}, Rows: []Row{{Cells: []string{"aaaa", "mid", "zzzz"}}}}
	got, _ := drawT(10, 1, tb, false)
	// 10 - 2 gaps = 8: 4 + 4 fixed, so the flex column gets nothing.
	if got != "aaaa  zzzz" {
		t.Fatalf("got %q, want the flexible column starved", got)
	}
	// Degenerate input never panics.
	DrawTable(New(0, 0), tb, false)
	DrawTable(New(5, 5), Table{}, false)
	DrawTable(New(1, 1), tb, false)
}
