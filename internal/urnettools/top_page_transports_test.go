package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// transportsModel renders the Transports page over one window: prev is the
// reading a window back and cur the newest one. Both are full blocks, so a
// kind that carried nothing in the window is still an enabled kind.
func transportsModel(t *testing.T, th tui.Theme, w, h int, span time.Duration, prev, cur *LiveTransports) (*topModel, string) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(th, topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.transportsHistory.push(transportTestBase, prev)
	m.transportsHistory.push(transportTestBase+nanos(span), cur)
	i := pageByID("transports")
	if i < 0 {
		t.Fatal("the transports page is not in the table")
	}
	m.page = i
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b.String()
}

// mixedWindow is the app's shape: H3 carries most of it, H1 a third, the
// separate H1+ carrier a small share, WhoDis a trace, and the pump nothing.
func mixedWindow() (prev, cur *LiveTransports) {
	const mb = 1 << 20
	prev = transportBlock(0, 0, 0, 0, 0)
	cur = transportBlock(60*mb, 30*mb, 9*mb, mb, 0)
	cur.H1WebSocket.Up = 20
	cur.H1Plus.Up = 5
	return prev, cur
}

// The page shows the window's share as a bar, a legend with whole percents, the
// unused footer and the table, and the percents add up to 100.
func TestTopTransportsPageDrawsTheWindowShares(t *testing.T) {
	prev, cur := mixedWindow()
	m, text := transportsModel(t, tui.DefaultTheme(), 120, 40, time.Minute, prev, cur)

	for _, want := range []string{
		"Transports",
		"last 60s",
		"● H3 60%",
		"● H1 30%",
		"● H1+ 9%",
		"● whodis 1%",
		"unused ",
		"○ whodis pump",
		"transport",
		"share",
		"rate",
		"conns",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("page does not show %q:\n%s", want, text)
		}
	}
	// The bar is drawn on the row under the caption and covers it completely.
	rows := strings.Split(text, "\n")
	barRow := -1
	for i, row := range rows {
		if strings.Contains(row, "last 60s") {
			barRow = i + 1
			break
		}
	}
	if barRow < 0 || barRow >= len(rows) {
		t.Fatalf("no caption row found:\n%s", text)
	}
	bar := rows[barRow]
	for _, glyph := range []rune("█▌▐") {
		if !strings.ContainsRune(bar, glyph) {
			t.Fatalf("the bar row holds no %q: %q", glyph, bar)
		}
	}
	// The segments tile the interior with no gap and no overflow.
	tiled := 0
	for _, r := range bar {
		if strings.ContainsRune("█▌▐░", r) {
			tiled++
		}
	}
	if want := 120 - 2; tiled != want {
		t.Fatalf("the bar covers %d cells, want the %d inside the frame: %q", tiled, want, bar)
	}

	shares, _, _, ready, ok := m.transportsData()
	if !ok || !ready {
		t.Fatalf("data ok=%v ready=%v, want both", ok, ready)
	}
	sum := 0
	for _, s := range shares {
		sum += s.Percent
	}
	if sum != 100 {
		t.Fatalf("whole percents sum to %d, want 100", sum)
	}
}

// The bar covers the content width exactly, with at least one cell for every
// kind that carried traffic, at every size and theme.
func TestTopTransportsPageBarTilesTheWidthExactly(t *testing.T) {
	prev, cur := mixedWindow()
	shares := transportWindowShares(prev, cur)
	for _, width := range []int{8, 20, 40, 78, 118} {
		cells := transportBarCells(shares, width)
		sum := 0
		for i, n := range cells {
			if n < 0 {
				t.Fatalf("width %d: negative cells for kind %d", width, i)
			}
			sum += n
		}
		if sum != width {
			t.Fatalf("width %d: cells sum to %d", width, sum)
		}
		if width >= 4 {
			for i, s := range shares {
				if s.Bytes > 0 && cells[i] == 0 {
					t.Fatalf("width %d: %s carried traffic but got no cell", width, s.Kind)
				}
			}
		}
	}
	for _, c := range []struct {
		name string
		w, h int
	}{
		{"120x40", 120, 40},
		{"80x24", 80, 24},
		{"72x20", 72, 20},
	} {
		m, text := transportsModel(t, tui.DefaultTheme(), c.w, c.h, time.Minute, prev, cur)
		_ = m
		for i, row := range strings.Split(text, "\n") {
			if len([]rune(row)) > c.w {
				t.Fatalf("%s: row %d is %d cells wide: %q", c.name, i, len([]rune(row)), row)
			}
		}
		if !strings.Contains(text, "H3") {
			t.Fatalf("%s: no legend in:\n%s", c.name, text)
		}
	}
}

// An idle window says so and lists every enabled kind as unused, because none
// of them carried anything.
func TestTopTransportsPageIdleWindow(t *testing.T) {
	block := transportBlock(1<<20, 2<<20, 0, 0, 0)
	_, text := transportsModel(t, tui.DefaultTheme(), 120, 40, time.Minute, block, block)
	if !strings.Contains(text, "idle") {
		t.Fatalf("an idle window does not say idle:\n%s", text)
	}
	for _, kind := range []string{"H3", "H1", "H1+", "whodis", "whodis pump"} {
		if !strings.Contains(text, "○ "+kind) {
			t.Fatalf("an idle window leaves %s out of the unused footer:\n%s", kind, text)
		}
	}
	if strings.Contains(text, "●") {
		t.Fatalf("an idle window shows a used marker:\n%s", text)
	}
	// The bar itself is the empty fill, one row of it under the caption.
	rows := strings.Split(text, "\n")
	barRow := -1
	for i, row := range rows {
		if strings.Contains(row, "idle") {
			barRow = i - 2
			break
		}
	}
	if barRow < 0 || barRow >= len(rows) {
		t.Fatalf("no idle caption found:\n%s", text)
	}
	bar := rows[barRow]
	if n := strings.Count(bar, "░"); n != 120-2 {
		t.Fatalf("the idle bar fills %d cells, want %d: %q", n, 120-2, bar)
	}
}

// A counter that went down is skipped rather than read as negative traffic: the
// kind is unused and the rest of the window keeps its own shares.
func TestTopTransportsPageSkipsADecrease(t *testing.T) {
	const mb = 1 << 20
	prev := transportBlock(100*mb, 0, 0, 0, 0)
	cur := transportBlock(50*mb, 10*mb, 0, 0, 0)
	m, text := transportsModel(t, tui.DefaultTheme(), 120, 40, time.Minute, prev, cur)
	shares, _, _, _, ok := m.transportsData()
	if !ok {
		t.Fatal("no data")
	}
	for _, s := range shares {
		switch s.Kind {
		case "h3":
			if s.Used || s.Percent != 0 {
				t.Fatalf("h3 read as %d%% after its counter went down", s.Percent)
			}
		case "h1_websocket":
			if !s.Used || s.Percent != 100 {
				t.Fatalf("h1 read as %d%%, want the whole window", s.Percent)
			}
		}
	}
	if !strings.Contains(text, "○ H3") {
		t.Fatalf("a decreased counter is not in the unused footer:\n%s", text)
	}
	if !strings.Contains(text, "● H1 100%") {
		t.Fatalf("the kind that carried the window is not the whole legend:\n%s", text)
	}
}

// H1 and H1+ are two carriers: they take separate segments and separate rows,
// and the H1 line says how many live H1 connections run on the H1+ carrier.
func TestTopTransportsPageSplitsH1FromH1Plus(t *testing.T) {
	const mb = 1 << 20
	prev := transportBlock(0, 0, 0, 0, 0)
	cur := transportBlock(0, 50*mb, 50*mb, 0, 0)
	cur.H1WebSocket.Up = 15
	cur.H1Plus.Up = 5
	_, text := transportsModel(t, tui.DefaultTheme(), 120, 40, time.Minute, prev, cur)
	for _, want := range []string{"● H1 50%", "● H1+ 50%", "H1 connections: 20, of them 5 on the H1+ carrier (25%)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("page does not show %q:\n%s", want, text)
		}
	}
	// The two carriers are their own bar runs, so both colors appear.
	shares := transportWindowShares(prev, cur)
	cells := transportBarCells(shares, 40)
	for i, s := range shares {
		if (s.Kind == "h1_websocket" || s.Kind == "h1_plus") && cells[i] < 1 {
			t.Fatalf("%s has no bar segment", s.Kind)
		}
	}
}

// Mono and ASCII draw the same page with the plain glyphs, and an old provider
// is left out of the strip and off the number keys.
func TestTopTransportsPageThemesAndHiddenPage(t *testing.T) {
	prev, cur := mixedWindow()
	for _, name := range []string{"mono", "colorblind"} {
		th := mustTheme(t, name)
		_, text := transportsModel(t, th, 100, 30, time.Minute, prev, cur)
		want := "● H3 60%"
		if th.ASCII {
			want = "* H3 60%"
		}
		if !strings.Contains(text, want) {
			t.Fatalf("%s: no legend entry %q:\n%s", name, want, text)
		}
		if th.ASCII && strings.ContainsAny(text, "█▌▐●○") {
			t.Fatalf("%s: an ASCII theme drew a block or dot glyph:\n%s", name, text)
		}
	}

	// A model with no transport readings: no page in the strip, and the digit
	// that would select it changes nothing.
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	idx := pageByID("transports")
	if idx < 0 {
		t.Fatal("the transports page is not in the table")
	}
	strip, _ := m.pageStrip(m.pageArea())
	for _, i := range strip {
		if i == idx {
			t.Fatalf("an old provider still offers the transports page")
		}
	}
	m.page = 0
	pressRune(m, rune('1'+idx))
	if m.page != 0 {
		t.Fatalf("the digit for an unavailable transports page selected %d", m.page)
	}
}
