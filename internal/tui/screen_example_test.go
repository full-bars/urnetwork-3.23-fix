package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// This file composes the widgets into the screen sketched in
// docs/design/urnet-tools-top.md, from fixed data, as the proof that the
// widget layer is enough to build `top` and as the golden-frame test bed. The
// real screen will live with the terminal backend; nothing here is imported by
// production code.

var update = flag.Bool("update", false, "rewrite golden files in testdata")

// exampleMin is the smallest terminal the full layout is designed for; below
// it the compact single-panel view is drawn instead.
var exampleMin = MinSize{W: 72, H: 20}

type exampleProxy struct {
	Name     string
	Rate     float64
	Sessions int
}

type exampleData struct {
	Node, Version, Verdict, Clock string
	Uptime                        time.Duration
	Samples                       []float64
	Billable, Avg1m, Avg5m        float64
	Clients                       int
	Pressure                      float64
	Up, Degraded, Connecting      int
	Dead                          int
	Top                           []exampleProxy
	Heap, HeapLimit               float64
	FDs, FDLimit, Goroutines      int
	Events                        []string
}

// exampleSamples builds a deterministic 10 minute series at one sample per
// second: a slow triangle wave with a little integer noise. It avoids math
// functions so the golden files do not depend on the platform's floating point.
func exampleSamples() []float64 {
	const mib = 1024 * 1024
	out := make([]float64, 600)
	seed := uint32(12345)
	for i := range out {
		phase := i % 240
		if phase > 120 {
			phase = 240 - phase
		}
		seed = seed*1664525 + 1013904223
		noise := int(seed>>24) % 6
		out[i] = float64(phase*int(mib)/6+noise*int(mib)/4) + 8*mib
	}
	return out
}

func exampleFixture() exampleData {
	const mib, gib = 1024 * 1024, 1024 * 1024 * 1024
	return exampleData{
		Node: "tornado", Version: "v3.23.0-fix.32.1", Verdict: "FLOWING", Clock: "12:04:31",
		Uptime:   3*24*time.Hour + 4*time.Hour + 20*time.Minute,
		Samples:  exampleSamples(),
		Billable: 38.2 * mib, Avg1m: 41.0 * mib, Avg5m: 35.4 * mib,
		Clients: 212, Pressure: 0.21,
		Up: 58, Degraded: 2,
		Top: []exampleProxy{
			{"proxy-a", 4.1 * mib, 31}, {"proxy-b", 3.8 * mib, 27},
			{"proxy-c", 3.2 * mib, 22}, {"proxy-with-a-very-long-name", 2.9 * mib, 19},
		},
		Heap: 1.8 * gib, HeapLimit: 4 * gib,
		FDs: 310, FDLimit: 65536, Goroutines: 1204,
		Events: []string{
			"12:03 proxy-c recovered",
			"12:01 restart: update to v3.23.0-fix.32.1",
			"11:47 proxy-d lost (dial timeout)",
		},
	}
}

// kv draws "key   value" on row y of b with a fixed key column.
func kv(b *Buffer, th Theme, y int, key, value string, valueStyle Style) {
	b.Put(key, 0, y, th.Dim)
	b.Put(value, 11, y, valueStyle)
}

func drawExampleScreen(b *Buffer, th Theme, d exampleData) {
	if !exampleMin.FitsRect(b.Rect()) {
		drawExampleCompact(b, th, d)
		return
	}
	rows := SplitRows(b.Rect(), Fixed(1), Flex(1), Fixed(1))
	header, body, footer := b.Sub(rows[0]), rows[1], b.Sub(rows[2])

	// The verdict and clock keep their place on the right; the node details
	// give way (ellipsis) when the screen is narrow.
	right := d.Verdict + "    " + d.Clock + " "
	rx := b.Width() - StringWidth(right)
	header.Put(d.Verdict, rx, 0, th.OK)
	header.Put("    "+d.Clock+" ", rx+StringWidth(d.Verdict), 0, th.Dim)
	x := header.Put(" urnet-tools top", 0, 0, th.Accent)
	details := "   node: " + d.Node + "   " + d.Version + "   up " + Duration(d.Uptime)
	header.Put(Truncate(details, rx-x-1, th.ASCII), x, 0, th.Dim)

	footer.Put(" q quit   tab provider   p proxies   r refresh rate   ? help", 0, 0, th.Dim)

	cols := SplitCols(body, Flex(1), Fixed(30))
	left := SplitRows(cols[0], Flex(1), Fixed(7), Fixed(5))
	side := SplitRows(cols[1], Fixed(8), Flex(1))
	box := func(r Rect, title string) *Buffer {
		return DrawBox(b.Sub(r), title, th.Frame, th.Border, th.Accent, th.ASCII)
	}

	DrawGraph(box(left[0], "Throughput, 10 min"), Graph{
		Samples: d.Samples, Binary: true, Format: RateShort, Style: th.Graph, AxisStyle: th.Dim,
	}, th.ASCII)

	px := box(left[1], "Proxies")
	total := d.Up + d.Degraded + d.Connecting + d.Dead
	DrawBar(px.Sub(Rect{W: px.Width(), H: 1}), Bar{
		Label: "up", LabelWidth: 8, Value: Percent(float64(d.Up) / float64(total)),
		Frac: float64(d.Up) / float64(total), LabelStyle: th.Dim, Fill: th.OK, Empty: th.Dim,
	}, th.ASCII)
	n := px.Put("degraded ", 0, 1, th.Dim)
	n = px.Put(Percent(float64(d.Degraded)/float64(total)), n, 1, th.Warn)
	n = px.Put("  connecting ", n, 1, th.Dim)
	n = px.Put(Percent(float64(d.Connecting)/float64(total)), n, 1, th.OK)
	n = px.Put("  dead ", n, 1, th.Dim)
	px.Put(Percent(float64(d.Dead)/float64(total)), n, 1, th.OK)
	tableRows := make([]Row, len(d.Top))
	for i, p := range d.Top {
		tableRows[i] = Row{Cells: []string{p.Name, Rate(p.Rate), strconv.Itoa(p.Sessions)}}
	}
	DrawTable(px.Sub(Rect{Y: 2, W: px.Width(), H: px.Height() - 2}), Table{
		Header: true, HeaderStyle: th.Dim, Rows: tableRows,
		Columns: []Column{{Title: "top by billable/s"}, {Title: "rate", Width: 11, Align: AlignRight}, {Title: "sess", Width: 4, Align: AlignRight}},
	}, th.ASCII)

	ev := box(left[2], "Events")
	for i, e := range d.Events {
		ev.Put(e, 0, i, th.Dim)
	}

	now := box(side[0], "Now")
	kv(now, th, 0, "billable", Rate(d.Billable), th.OK)
	kv(now, th, 1, "1m avg", Rate(d.Avg1m), Style{})
	kv(now, th, 2, "5m avg", Rate(d.Avg5m), Style{})
	kv(now, th, 3, "clients", strconv.Itoa(d.Clients), Style{})
	DrawBar(now.Sub(Rect{Y: 4, W: now.Width(), H: 1}), Bar{
		Label: "pressure", LabelWidth: 10, Value: fmt.Sprintf("%.2f", d.Pressure), Frac: d.Pressure,
		LabelStyle: th.Dim, Fill: th.Level(d.Pressure, 0.7, 0.9), Empty: th.Dim,
	}, th.ASCII)

	res := box(side[1], "Resources")
	const gib = 1024 * 1024 * 1024
	heap := d.Heap / d.HeapLimit
	DrawBar(res.Sub(Rect{W: res.Width(), H: 1}), Bar{
		Label: "heap", LabelWidth: 5, Value: fmt.Sprintf("%.1f/%.1f GiB", d.Heap/gib, d.HeapLimit/gib), Frac: heap,
		LabelStyle: th.Dim, Fill: th.Level(heap, 0.7, 0.9), Empty: th.Dim,
	}, th.ASCII)
	fds := float64(d.FDs) / float64(d.FDLimit)
	DrawBar(res.Sub(Rect{Y: 1, W: res.Width(), H: 1}), Bar{
		Label: "fds", LabelWidth: 5, Value: strconv.Itoa(d.FDs) + "/" + strconv.Itoa(d.FDLimit), Frac: fds,
		LabelStyle: th.Dim, Fill: th.Level(fds, 0.7, 0.9), Empty: th.Dim,
	}, th.ASCII)
	res.Put("gor", 0, 2, th.Dim)
	res.Put(strconv.Itoa(d.Goroutines), 6, 2, Style{})
}

// drawExampleCompact is the single-panel view for terminals below exampleMin.
func drawExampleCompact(b *Buffer, th Theme, d exampleData) {
	in := DrawBox(b, "urnet-tools top", th.Frame, th.Border, th.Accent, th.ASCII)
	if in.Height() == 0 {
		return
	}
	in.Put(d.Verdict, 0, 0, th.OK)
	in.Put(" "+Rate(d.Billable), len(d.Verdict), 0, Style{})
	in.Put("clients "+strconv.Itoa(d.Clients)+"  up "+strconv.Itoa(d.Up)+"/"+strconv.Itoa(d.Up+d.Degraded+d.Connecting+d.Dead), 0, 1, th.Dim)
	vals := make([]uint64, len(d.Samples))
	for i, v := range d.Samples {
		vals[i] = uint64(v)
	}
	in.Put(Spark(vals, in.Width(), th.ASCII), 0, 2, th.Graph)
	in.Put("enlarge the terminal for the full view", 0, in.Height()-1, th.Dim)
}

// checkGolden compares got against testdata/<name>.golden, or rewrites the file
// under -update. The stored text ends in a newline; Buffer.String() does not.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	got += "\n"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !sameGolden(got, want) {
		t.Fatalf("%s differs from the golden file (run with -update to accept):\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

// sameGolden compares rendered output with a golden file, treating CRLF pairs
// in the file as LF: a Windows checkout with core.autocrlf rewrites it, and
// that is not a difference in what is rendered.
func sameGolden(got string, want []byte) bool {
	return got == strings.ReplaceAll(string(want), "\r\n", "\n")
}

func TestExampleScreenGolden(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		theme   Theme
		compact bool
	}{
		{"screen_120x40", 120, 40, DefaultTheme(), false},
		{"screen_80x24", 80, 24, DefaultTheme(), false},
		{"screen_compact_40x10", 40, 10, DefaultTheme(), true},
		{"screen_mono_120x40", 120, 40, MonoTheme(), false},
		{"screen_mono_80x24", 80, 24, MonoTheme(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if fits := exampleMin.Fits(c.w, c.h); fits == c.compact {
				t.Fatalf("Fits(%dx%d) = %v, but compact is %v", c.w, c.h, fits, c.compact)
			}
			b := New(c.w, c.h)
			drawExampleScreen(b, c.theme, exampleFixture())
			got := b.String()
			checkGolden(t, c.name, got)

			for i, line := range strings.Split(got, "\n") {
				if w := StringWidth(line); w > c.w {
					t.Fatalf("row %d is %d cells wide in a %d wide screen", i, w, c.w)
				}
			}
			// Drawing is a pure function of its inputs.
			b2 := New(c.w, c.h)
			drawExampleScreen(b2, c.theme, exampleFixture())
			if b2.String() != got || len(Diff(b, b2)) != 0 {
				t.Fatal("two draws of the same data differ")
			}
		})
	}
}

func TestExampleScreenMonoIsPlain(t *testing.T) {
	b := New(120, 40)
	drawExampleScreen(b, MonoTheme(), exampleFixture())
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y)
			if c.Style != (Style{}) {
				t.Fatalf("mono cell (%d,%d) carries style %+v", x, y, c.Style)
			}
			if c.Rune > 0x7E {
				t.Fatalf("mono cell (%d,%d) is non-ascii %q", x, y, c.Rune)
			}
		}
	}
}

func TestExampleScreenDefaultThemeUsesRoles(t *testing.T) {
	th := DefaultTheme()
	b := New(120, 40)
	drawExampleScreen(b, th, exampleFixture())
	if b.Cell(0, 1).Style != th.Border || b.Cell(0, 1).Rune != '╭' {
		t.Fatalf("panel corner = %q %+v", b.Cell(0, 1).Rune, b.Cell(0, 1).Style)
	}
	if !strings.Contains(b.String(), "⣿") {
		t.Fatal("default theme should draw the braille graph")
	}
}

// Every size from unusable to large must draw without panicking and without
// leaving half a wide rune anywhere.
func TestExampleScreenAnySize(t *testing.T) {
	for w := 0; w <= 130; w += 7 {
		for h := 0; h <= 45; h += 3 {
			for _, th := range []Theme{DefaultTheme(), MonoTheme()} {
				b := New(w, h)
				drawExampleScreen(b, th, exampleFixture())
				if !utf8.ValidString(b.String()) {
					t.Fatalf("%dx%d: invalid utf8", w, h)
				}
			}
		}
	}
}

// Same rule as the urnettools golden helper: a checkout that converts line
// endings (Windows runners default to core.autocrlf) must not fail the compare.
func TestSameGoldenIgnoresLineEndingConversion(t *testing.T) {
	const lf = "row one\nrow two\n"
	if !sameGolden(lf, []byte("row one\r\nrow two\r\n")) {
		t.Fatal("a golden file checked out with CRLF must still match LF output")
	}
	if sameGolden(lf, []byte("row one\nrow TWO\n")) {
		t.Fatal("a real difference must still fail")
	}
}
