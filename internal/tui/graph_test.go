package tui

import (
	"math"
	"strings"
	"testing"
)

func TestNiceCeil(t *testing.T) {
	cases := []struct {
		in     float64
		binary bool
		want   float64
	}{
		{0, false, 0}, {-3, false, 0}, {math.NaN(), false, 0}, {math.Inf(1), false, 0},
		{1, false, 1}, {0.7, false, 0.8}, {41.3, false, 50}, {100, false, 100}, {101, false, 120},
		{1000, false, 1000}, {999.99, false, 1000}, {7, false, 8}, {9.1, false, 10},
		{0.013, false, 0.015},
		{500 * 1024, true, 500 * 1024}, {488.3 * 1024, true, 500 * 1024},
		{1.1 * 1024 * 1024, true, 1.2 * 1024 * 1024}, {1023 * 1024, true, 1024 * 1024},
	}
	for _, c := range cases {
		got := niceCeil(c.in, c.binary)
		if math.Abs(got-c.want) > c.want*1e-9 {
			t.Errorf("niceCeil(%v, %v) = %v, want %v", c.in, c.binary, got, c.want)
		}
		if c.in > 0 && !math.IsInf(c.in, 0) && got < c.in*(1-1e-9) {
			t.Errorf("niceCeil(%v) = %v is below its input", c.in, got)
		}
	}
}

func drawG(w, h int, g Graph, ascii bool) (string, float64) {
	b := New(w, h)
	top := DrawGraph(b, g, ascii)
	return b.String(), top
}

func TestGraphRampBraille(t *testing.T) {
	// One row is 4 dots tall. Samples 1..8 against a top of 8 give 1,1,2,2,3,3,4,4
	// dots (half rounds up), two per cell, so the plot climbs one dot row per cell.
	got, top := drawG(6, 1, Graph{Samples: []float64{1, 2, 3, 4, 5, 6, 7, 8}, Max: 8}, false)
	if top != 8 {
		t.Fatalf("top = %v", top)
	}
	// A one-row graph has only its top label; the newest sample is the rightmost cell.
	if want := "8┤⣀⣤⣶⣿"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestGraphGolden(t *testing.T) {
	samples := []float64{0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 100, 50, 0}
	got, top := drawG(12, 4, Graph{Samples: samples}, false)
	if top != 100 {
		t.Fatalf("auto top = %v, want 100", top)
	}
	// 14 samples right-aligned into 16 columns: two blank columns on the left, a
	// baseline dot for the zero at each end, and the peak reaching the top row.
	want := strings.Join([]string{
		"100┤     ⣠⣿",
		"   │    ⣴⣿⣿",
		"   │  ⢀⣼⣿⣿⣿⡇",
		"  0┤ ⣠⣾⣿⣿⣿⣿⣇",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestGraphExactBraille(t *testing.T) {
	// A 2-sample series in a 1-row graph: heights of 4 and 2 dots against a
	// fixed max of 4 (top of axis).
	b := New(3, 1)
	DrawGraph(b, Graph{Samples: []float64{4, 2}, Max: 4}, false)
	// Axis: "4┤" is 2 cells, plot is 1 cell = 2 columns. Left column 4 dots,
	// right column 2 dots: left 0x40|0x04|0x02|0x01 = 0x47, right 0x80|0x20 = 0xA0.
	if got := b.Cell(2, 0).Rune; got != 0x2800+0x47+0xA0 {
		t.Fatalf("cell = %U, want %U", got, 0x2800+0x47+0xA0)
	}
}

func TestGraphNewestIsRightAligned(t *testing.T) {
	b := New(7, 2)
	DrawGraph(b, Graph{Samples: []float64{5}, Max: 5}, false)
	// One sample: it is the rightmost column of the rightmost cell, so the
	// cell holds only right-hand dots and every other plot cell is empty.
	last := b.Cell(6, 1).Rune
	if last&0x2800 != 0x2800 || (last-0x2800)&0x47 != 0 {
		t.Fatalf("newest sample not in the right half of the last cell: %U", last)
	}
	for x := 2; x < 6; x++ {
		for y := 0; y < 2; y++ {
			if b.Cell(x, y).Rune != ' ' {
				t.Fatalf("cell (%d,%d) drawn before any sample: %q", x, y, b.Cell(x, y).Rune)
			}
		}
	}
}

func TestGraphEmptyAndAllZero(t *testing.T) {
	got, top := drawG(8, 3, Graph{}, false)
	if top != 0 {
		t.Fatalf("empty top = %v", top)
	}
	if strings.ContainsAny(got, "⠀⣀⣿") {
		t.Fatalf("empty series drew data:\n%s", got)
	}
	if !strings.Contains(got, "0┤") || strings.Count(got, "┤") != 1 {
		t.Fatalf("empty series should label only zero:\n%s", got)
	}

	got, top = drawG(8, 2, Graph{Samples: []float64{0, 0, 0, 0}}, false)
	if top != 0 {
		t.Fatalf("all-zero top = %v", top)
	}
	lines := strings.Split(got, "\n")
	// The baseline is present on the bottom row and nothing above it.
	if strings.TrimSpace(strings.TrimLeft(lines[0], " │")) != "" {
		t.Fatalf("all-zero graph drew above the baseline:\n%s", got)
	}
	if !strings.ContainsRune(lines[1], '⣀') {
		t.Fatalf("all-zero graph should draw a baseline:\n%s", got)
	}
}

func TestGraphAveragesLongSeries(t *testing.T) {
	long := make([]float64, 600)
	for i := range long {
		long[i] = float64(i % 10)
	}
	b := New(30, 5)
	DrawGraph(b, Graph{Samples: long}, false)
	// The whole window shows: the leftmost plot column is drawn, not blank.
	if b.Cell(3, 4).Rune == ' ' {
		t.Fatal("leftmost plot cell is blank: the long series was cut instead of averaged down")
	}
	cols := plotColumns([]float64{0, 0, 10, 10}, 2)
	if cols[0] != 0 || cols[1] != 10 {
		t.Fatalf("bucket average: %v", cols)
	}
	cols = plotColumns([]float64{1, 2}, 4)
	if !math.IsNaN(cols[0]) || !math.IsNaN(cols[1]) || cols[2] != 1 || cols[3] != 2 {
		t.Fatalf("right alignment: %v", cols)
	}
}

func TestGraphSanitizesSamples(t *testing.T) {
	b := New(10, 2)
	top := DrawGraph(b, Graph{Samples: []float64{math.NaN(), math.Inf(1), -5, 3}}, false)
	if top != 3 {
		t.Fatalf("top = %v, non-finite and negative samples must count as zero", top)
	}
}

func TestGraphMiddleTickOnlyWhenTall(t *testing.T) {
	got, _ := drawG(10, 5, Graph{Samples: []float64{100}, Max: 100}, false)
	if strings.Count(got, "┤") != 3 || !strings.Contains(got, "50┤") {
		t.Fatalf("5-row graph should label top, middle and bottom:\n%s", got)
	}
	got, _ = drawG(10, 4, Graph{Samples: []float64{100}, Max: 100}, false)
	if strings.Count(got, "┤") != 2 {
		t.Fatalf("4-row graph should label only top and bottom:\n%s", got)
	}
}

func TestGraphFormatAndBinary(t *testing.T) {
	g := Graph{Samples: []float64{300 * 1024}, Binary: true, Format: RateShort}
	got, top := drawG(20, 3, g, false)
	if top != 300*1024 {
		t.Fatalf("top = %v", top)
	}
	if !strings.Contains(got, "300 KiB/s┤") || !strings.Contains(got, "0 B/s┤") {
		t.Fatalf("labels:\n%s", got)
	}
}

func TestGraphASCII(t *testing.T) {
	got, _ := drawG(9, 2, Graph{Samples: []float64{0, 0, 4, 4, 8, 8, 8, 8}, Max: 8, Format: func(v float64) string { return "" }}, true)
	if strings.ContainsAny(got, "⣀⣿┤│") {
		t.Fatalf("ascii graph contains unicode:\n%s", got)
	}
	for _, r := range got {
		if r > 0x7E {
			t.Fatalf("non-ascii %q in\n%s", r, got)
		}
	}
	lines := strings.Split(got, "\n")
	// Newest column is full height: '#' in both rows. Oldest is the baseline.
	if !strings.HasSuffix(lines[0], "#") || !strings.HasSuffix(lines[1], "#") {
		t.Fatalf("full-height newest column expected:\n%s", got)
	}
	if !strings.Contains(lines[1], "_") {
		t.Fatalf("zero sample should draw a baseline glyph:\n%s", got)
	}
}

func TestGraphTinyRegions(t *testing.T) {
	for _, dim := range [][2]int{{0, 0}, {1, 1}, {2, 1}, {3, 1}, {1, 5}, {5, 1}} {
		b := New(dim[0], dim[1])
		DrawGraph(b, Graph{Samples: []float64{1, 5, 3, 9, 2}}, false)
		DrawGraph(b, Graph{Samples: []float64{1, 5, 3, 9, 2}}, true)
	}
	// With no room for a plot beside the axis, the axis is dropped and data is drawn.
	b := New(2, 1)
	DrawGraph(b, Graph{Samples: []float64{9, 9}, Format: func(float64) string { return "9999" }}, false)
	if b.Cell(1, 0).Rune < 0x2800 {
		t.Fatalf("plot should win over the axis in a tiny region: %q", b.String())
	}
}

// series returns the n samples ending at absolute time end, where the sample at
// time a has a fixed value regardless of when it is read. That is what a rate
// history is: a new second adds one sample and the oldest falls off, and no
// existing sample changes.
func series(end int64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		a := end - int64(n-1-i)
		out[i] = float64((a*7919 + a%13*104729) % 1000)
	}
	return out
}

// The graph used to bucket from the oldest sample, so once the ring was full
// every second shifted every bucket boundary and the whole graph re-averaged
// (shimmered). Buckets aligned to absolute time must never change once
// complete: only the newest, still-filling bucket may differ between frames.
func TestGraphCompletedBucketsNeverChange(t *testing.T) {
	const n, ring = 60, 600
	seen := map[int64]float64{}
	for end := int64(1_000_000); end < 1_000_000+400; end++ {
		ids, vals := bucketColumns(series(end, ring), n, end, ring)
		if len(ids) == 0 || len(ids) > n {
			t.Fatalf("end=%d: %d columns for %d slots", end, len(ids), n)
		}
		for i := 0; i < len(ids)-1; i++ { // every bucket but the newest is complete
			if prev, ok := seen[ids[i]]; ok && prev != vals[i] {
				t.Fatalf("end=%d: completed bucket %d changed from %v to %v (the graph shimmered)", end, ids[i], prev, vals[i])
			}
			seen[ids[i]] = vals[i]
		}
	}
}

func TestGraphAnchoredScrollsOneColumnAtATime(t *testing.T) {
	const n, ring, seconds = 60, 600, 400
	var prevIDs []int64
	shifts, holds := 0, 0
	for end := int64(2_000_000); end < 2_000_000+seconds; end++ {
		ids, _ := bucketColumns(series(end, ring), n, end, ring)
		if prevIDs != nil {
			switch d := ids[len(ids)-1] - prevIDs[len(prevIDs)-1]; d {
			case 0:
				holds++
			case 1:
				shifts++
			default:
				t.Fatalf("end=%d: newest bucket id jumped by %d", end, d)
			}
		}
		prevIDs = ids
	}
	// 600 samples over 60 columns is 10 seconds a column: one scroll every ten
	// seconds and holds in between, a slow steady scroll, not a redraw a second.
	if shifts < seconds/10-2 || shifts > seconds/10+2 || holds < seconds-shifts-2 {
		t.Fatalf("shifts=%d holds=%d over %d seconds", shifts, holds, seconds)
	}
}

func TestGraphAnchorZeroKeepsTheOldLayout(t *testing.T) {
	// Callers with no time base (and the existing tests) get the same layout as
	// before: an unanchored series is bucketed from the oldest sample.
	cols := plotColumnsAnchored([]float64{0, 0, 10, 10}, 2, 0, 0)
	if cols[0] != 0 || cols[1] != 10 {
		t.Fatalf("unanchored bucket average: %v", cols)
	}
}

// A graph must fill its width. Integer bucket sizes cannot always match the
// column count, so a wide plot must show the newest columns that fit rather than
// leave a third of the graph blank.
func TestGraphAnchoredFillsWideAndNarrowPlots(t *testing.T) {
	for _, n := range []int{20, 60, 100, 124, 162, 200, 299, 400} {
		ids, vals := bucketColumns(series(5_000_000, 600), n, 5_000_000, 600)
		if len(ids) > n {
			t.Fatalf("n=%d: %d columns overflow the plot", n, len(ids))
		}
		if len(ids) < n-1 {
			t.Fatalf("n=%d: only %d columns drawn, the graph would show a blank stretch", n, len(ids))
		}
		if len(ids) != len(vals) {
			t.Fatalf("n=%d: %d ids for %d values", n, len(ids), len(vals))
		}
	}
}

func TestGraphAnchoredWideStaysStableWhileScrolling(t *testing.T) {
	const n, ring = 299, 600
	seen := map[int64]float64{}
	for end := int64(7_000_000); end < 7_000_000+300; end++ {
		ids, vals := bucketColumns(series(end, ring), n, end, ring)
		for i := 0; i < len(ids)-1; i++ {
			if prev, ok := seen[ids[i]]; ok && prev != vals[i] {
				t.Fatalf("end=%d: completed bucket %d changed from %v to %v", end, ids[i], prev, vals[i])
			}
			seen[ids[i]] = vals[i]
		}
	}
}

// A young series (a provider that only just started) must not re-bucket as it
// fills: the width is fixed by the capacity, so completed columns hold still
// from the very first minute and the graph grows in from the right.
func TestGraphAnchoredIsStableWhileTheSeriesFills(t *testing.T) {
	const n, capacity = 120, 600
	seen := map[int64]float64{}
	for length := 3; length <= capacity+40; length++ {
		end := int64(9_000_000) + int64(length) // one new sample a second
		ids, vals := bucketColumns(series(end, min(length, capacity)), n, end, capacity)
		for i := 0; i < len(ids)-1; i++ {
			if prev, ok := seen[ids[i]]; ok && prev != vals[i] {
				t.Fatalf("length=%d: completed bucket %d changed from %v to %v while filling", length, ids[i], prev, vals[i])
			}
			seen[ids[i]] = vals[i]
		}
	}
}

func TestGraphAnchoredYoungSeriesGrowsInFromTheRight(t *testing.T) {
	// 30 seconds of history on a plot sized for 600: a few columns at the right
	// edge, the rest empty (NaN), not the 30 samples stretched across the plot.
	cols := plotColumnsAnchored(series(4_000_000, 30), 120, 4_000_000, 600)
	drawn := 0
	for _, v := range cols {
		if !math.IsNaN(v) {
			drawn++
		}
	}
	if drawn == 0 || drawn > 10 || math.IsNaN(cols[len(cols)-1]) {
		t.Fatalf("%d of %d columns drawn for 30s of history, newest at the right edge: %v", drawn, len(cols), math.IsNaN(cols[len(cols)-1]))
	}
}

// The live rate is drawn as its own newest column. It is not part of the bucketed
// series, so it cannot change any column before it, and it never sets the scale:
// a burst in the live column must not rescale the whole chart 10 times a second.
func TestGraphTailIsTheNewestColumnAndNeverChangesTheRest(t *testing.T) {
	const w, h = 40, 6
	samples := series(6_000_000, 600)
	draw := func(tail float64) (string, float64) {
		b := New(w, h)
		top := DrawGraph(b, Graph{Samples: samples, Anchor: 6_000_000, Capacity: 600, Tail: tail, HasTail: true}, false)
		return b.String(), top
	}
	// Buffer.String trims trailing blanks, so pad each row to the full width
	// before cutting off the rightmost cell.
	dropRight := func(s string) []string {
		var rows []string
		for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			r := []rune(l)
			for len(r) < w {
				r = append(r, ' ')
			}
			rows = append(rows, string(r[:w-1]))
		}
		return rows
	}

	// With a tail always present, moving it (the live rate changes every 100ms)
	// changes only the rightmost cell; every column before it holds still.
	lowOut, lowTop := draw(1)
	midOut, midTop := draw(5 * 1024 * 1024)
	if lowOut == midOut {
		t.Fatal("the tail was not drawn")
	}
	a, b := dropRight(lowOut), dropRight(midOut)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("row %d changed outside the rightmost cell when only the tail moved:\n%q\n%q", i, a[i], b[i])
		}
	}

	// A huge tail does not rescale the chart: the scale is unchanged and the tail
	// is clamped to the plot instead of overflowing it.
	hugeOut, hugeTop := draw(1e12)
	if lowTop != midTop || midTop != hugeTop {
		t.Fatalf("the tail changed the scale: %v %v %v", lowTop, midTop, hugeTop)
	}
	if got := len([]rune(strings.Split(hugeOut, "\n")[0])); got != w {
		t.Fatalf("a huge tail changed the plot width to %d", got)
	}
	c := dropRight(hugeOut)
	for i := range a {
		if a[i] != c[i] {
			t.Fatalf("row %d changed outside the rightmost cell for a huge tail", i)
		}
	}
}

func TestGraphTailIgnoresBadValues(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), -5} {
		b := New(20, 4)
		DrawGraph(b, Graph{Samples: []float64{1, 2, 3}, Tail: v, HasTail: true}, false)
	}
}

// In the ASCII graph one cell holds a pair of samples and is drawn as their
// average. The live tail must own its whole cell: sharing one with the newest
// history sample showed a live spike at a blended, lower rate.
func TestGraphASCIITailOwnsItsCell(t *testing.T) {
	b := New(12, 2)
	DrawGraph(b, Graph{
		Samples: []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, Max: 10,
		Tail: 10, HasTail: true,
	}, true)
	last := b.Width() - 1
	// A tail at the axis top fills its cell on both rows. Averaged with a zero
	// history sample it would fill half of it.
	for row := 0; row < 2; row++ {
		if got := b.Cell(last, row).Rune; got != '#' {
			t.Fatalf("row %d of the rightmost cell = %q, want a full '#' (tail blended with history?):\n%s", row, got, b.String())
		}
	}
	// The cell before it is history and unaffected by the tail.
	for row := 0; row < 2; row++ {
		if got := b.Cell(last-1, row).Rune; got == '#' {
			t.Fatalf("the tail leaked into the previous cell at row %d:\n%s", row, b.String())
		}
	}
}

// The three styles draw the same data with different glyphs; each has its own
// resolution, and the fallbacks are what they say.
func TestGraphSymbolStyles(t *testing.T) {
	samples := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 10, 10, 5, 0, 0}
	draw := func(sym GraphSymbols, ascii bool) string {
		b := New(20, 4)
		DrawGraph(b, Graph{Samples: samples, Max: 10, Symbols: sym}, ascii)
		return b.String()
	}
	braille, block, tty := draw(GraphBraille, false), draw(GraphBlock, false), draw(GraphTTY, false)
	if !strings.ContainsAny(braille, "⣀⣿⣠⣴") || strings.ContainsAny(braille, "█▁") {
		t.Errorf("braille style:\n%s", braille)
	}
	if !strings.Contains(block, "█") || !strings.ContainsAny(block, "▁▂▃▄▅▆▇") || strings.ContainsAny(block, "⣀⣿") {
		t.Errorf("block style:\n%s", block)
	}
	if strings.ContainsAny(tty, "⣀⣿█▁") || !strings.Contains(tty, "#") {
		t.Errorf("tty style:\n%s", tty)
	}
	// A terminal that cannot draw non-ASCII overrides whatever was asked for.
	// The axis ticks differ (+ and | against ┤ and │), so only the plot, which
	// starts after the 2 wide labels and the tick, is compared.
	plot := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if r := []rune(l); len(r) > 3 {
				l = string(r[3:])
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	if got := draw(GraphBraille, true); plot(got) != plot(tty) {
		t.Errorf("ascii must force the tty style:\n%s\nvs\n%s", got, tty)
	}
	// Block gives each sample its own cell, so it shows more distinct columns
	// than the two-samples-per-cell styles for the same width.
	if strings.Count(block, "█")+strings.Count(block, "▁") == 0 {
		t.Error("block drew nothing")
	}
}

func TestGraphSymbolNames(t *testing.T) {
	for i, n := range GraphSymbolNames {
		got, ok := GraphSymbolsByName(n)
		if !ok || int(got) != i || got.String() != n {
			t.Errorf("%q round trip = %v %v", n, got, ok)
		}
	}
	if _, ok := GraphSymbolsByName("sparkles"); ok {
		t.Error("unknown style resolved")
	}
	if GraphSymbols(99).String() != "braille" {
		t.Error("an out-of-range style must read as the default")
	}
}

// Block with a tail: history takes every cell but the newest and the tail the
// last, exactly as braille does.
func TestGraphBlockTakesTheTailInItsLastCell(t *testing.T) {
	b := New(14, 3)
	DrawGraph(b, Graph{Samples: []float64{1, 1, 1, 1, 1}, Max: 10, Symbols: GraphBlock, Tail: 10, HasTail: true}, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	last := []rune(lines[0])
	if got := last[len(last)-1]; got != '█' {
		t.Fatalf("tail at the axis top should fill the last column, top row = %q", lines[0])
	}
}

// The rule is about pair-averaged cells, not about a flag: the TTY style on a
// terminal that can draw anything must give the tail its own cell too, while
// braille and block need nothing special.
func TestGraphTailOwnsItsCellInEveryStyle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sym   GraphSymbols
		ascii bool
		full  rune
	}{
		{"tty style", GraphTTY, false, '#'},
		{"ascii terminal", GraphBraille, true, '#'},
		{"block", GraphBlock, false, '█'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New(12, 2)
			DrawGraph(b, Graph{
				Samples: []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, Max: 10,
				Tail: 10, HasTail: true, Symbols: tc.sym,
			}, tc.ascii)
			last := b.Width() - 1
			for row := 0; row < 2; row++ {
				if got := b.Cell(last, row).Rune; got != tc.full {
					t.Fatalf("row %d of the rightmost cell = %q, want %q:\n%s", row, got, tc.full, b.String())
				}
			}
		})
	}
	// Braille: the tail is one dot column; the neighbouring dot column is history.
	b := New(12, 2)
	DrawGraph(b, Graph{Samples: []float64{0, 0, 0, 0, 0, 0}, Max: 10, Tail: 10, HasTail: true}, false)
	if got := b.Cell(b.Width()-1, 0).Rune; got == ' ' {
		t.Fatalf("braille tail not drawn:\n%s", b.String())
	}
}
