package urnettools

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// remoteGreen and remotePink stand in for the two curve colours.
var (
	remoteGreen = tui.Style{FG: tui.Xterm(120)}
	remotePink  = tui.Style{FG: tui.Xterm(213)}
)

// remoteTestChart builds a chart with the two halves the panel draws today and
// a fixed clock, so a render is a function of its samples alone.
func remoteTestChart(up, down []float64) remoteChart {
	th := tui.DefaultTheme()
	c := remoteChart{
		Now:      topBase,
		Newest:   topBase,
		Interval: time.Second,
		Window:   60 * time.Second,
		Scales:   map[string]*remoteScale{},
		Dim:      th.Dim,
		Text:     th.Accent,
	}
	if up != nil {
		c.Up = []remoteSeries{{Label: "billable", Style: remoteGreen, Group: "billable", Values: up}}
	}
	if down != nil {
		c.Down = []remoteSeries{{Label: "total", Style: remoteGreen, Group: "total", Values: down}}
	}
	return c
}

// remoteFlat is n samples all at v.
func remoteFlat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// remoteSpike is a flat series with a triangular peak of width 9 at index at.
func remoteSpike(n, at int, peak, base float64) []float64 {
	out := remoteFlat(n, base)
	for i := at - 4; i <= at+4; i++ {
		if i < 0 || i >= n {
			continue
		}
		step := 4 - absInt(i-at)
		out[i] = base + (peak-base)*float64(step)/4
	}
	return out
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// remoteRender draws a chart into a buffer of w by h and returns the text.
func remoteRender(c remoteChart, w, h int) string {
	b := tui.New(w, h)
	drawRemoteChart(b, c)
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func TestRemoteGoldenFlat(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 2048), remoteFlat(60, 4096))
	assertGolden(t, "top_remote_flat_60x10", remoteRender(c, 60, 10))
}

func TestRemoteGoldenPeaks(t *testing.T) {
	up := remoteSpike(60, 22, 72*1024, 900)
	down := remoteSpike(60, 40, 449*1024, 1400)
	assertGolden(t, "top_remote_peaks_60x10", remoteRender(remoteTestChart(up, down), 60, 10))
}

func TestRemoteGoldenEdgePeaks(t *testing.T) {
	// A peak in the first bucket and one in the last: both labels have to stay
	// inside the plot instead of hanging off an edge.
	up := remoteSpike(60, 1, 500*1024, 1200)
	down := remoteSpike(60, 58, 900*1024, 2000)
	assertGolden(t, "top_remote_edge_peaks_40x10", remoteRender(remoteTestChart(up, down), 40, 10))
}

func TestRemoteGoldenMono(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 20, 72*1024, 900), remoteSpike(60, 45, 300*1024, 1400))
	c.Up[0].Style, c.Down[0].Style = tui.Style{}, tui.Style{}
	c.ASCII = true
	c.Dim = tui.Style{}
	c.Text = tui.Style{}
	assertGolden(t, "top_remote_mono_60x10", remoteRender(c, 60, 10))
}

func TestRemoteGoldenASCII(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 20, 72*1024, 900), remoteSpike(60, 45, 300*1024, 1400))
	c.ASCII = true
	assertGolden(t, "top_remote_ascii_60x10", remoteRender(c, 60, 10))
}

func TestRemoteGoldenNarrow(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 20, 72*1024, 900), remoteSpike(60, 40, 300*1024, 1400))
	assertGolden(t, "top_remote_narrow_16x10", remoteRender(c, 16, 10))
}

func TestRemoteGoldenTiny(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 4096), remoteFlat(60, 8192))
	assertGolden(t, "top_remote_tiny_20x4", remoteRender(c, 20, 4))
	assertGolden(t, "top_remote_one_line_24x2", remoteRender(c, 24, 2))
}

func TestRemoteGoldenSingleSample(t *testing.T) {
	assertGolden(t, "top_remote_single_30x8", remoteRender(remoteTestChart([]float64{4096}, []float64{8192}), 30, 8))
}

func TestRemoteGoldenEmpty(t *testing.T) {
	assertGolden(t, "top_remote_empty_30x8", remoteRender(remoteTestChart(nil, nil), 30, 8))
	// A provider without the total figures still draws billable, and says the
	// other half is not reported rather than drawing a flat zero.
	c := remoteTestChart(remoteSpike(60, 20, 72*1024, 900), nil)
	assertGolden(t, "top_remote_billable_only_40x10", remoteRender(c, 40, 10))
}

// TestRemoteCorners checks the corner figures: the mean of the newest five
// samples per direction, and the arrow that lights up with traffic.
func TestRemoteCorners(t *testing.T) {
	up := []float64{1, 2, 3, 4, 5, 1024, 1024, 1024, 1024, 1024}
	down := []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if got := remoteAverage(up); got != 1024 {
		t.Fatalf("average of the newest five = %v, want 1024", got)
	}
	if got := remoteAverage(down); got != 0 {
		t.Fatalf("average of an idle direction = %v, want 0", got)
	}
	if got := remoteAverage(nil); got != 0 {
		t.Fatalf("average of no samples = %v, want 0", got)
	}
	// Short series average what it has.
	if got := remoteAverage([]float64{100, 300}); got != 200 {
		t.Fatalf("average of two samples = %v, want 200", got)
	}
	// Missed polls are skipped, not counted as zero.
	if got := remoteAverage([]float64{100, nan(), 300}); got != 200 {
		t.Fatalf("average with a gap = %v, want 200", got)
	}

	b := tui.New(40, 10)
	c := remoteTestChart(up, down)
	drawRemoteChart(b, c)
	got := strings.TrimRight(b.String(), "\n")
	if !strings.Contains(got, "1.0 KiB/s") || !strings.Contains(got, "▲") {
		t.Fatalf("no up average in the header:\n%s", got)
	}
	if !strings.Contains(got, "0 B/s") || !strings.Contains(got, "▼") {
		t.Fatalf("no down average in the footer:\n%s", got)
	}
}

func nan() float64 {
	var zero float64
	return zero / zero
}

// TestRemoteScaleEasing pins the easing curve at fixed times: it starts where
// the previous scale was, lands on the new peak after the ease, and uses the
// same ease out cubic the client app uses.
func TestRemoteScaleEasing(t *testing.T) {
	var s remoteScale
	start := topBase
	s.target(100, start)
	if got := s.valueAt(start); got != 100 {
		t.Fatalf("first target = %v, want 100", got)
	}
	s.target(200, start)
	cases := []struct {
		after time.Duration
		want  float64
	}{
		{0, 100},
		{remoteScaleEase / 2, 100 + 100*(1-0.125)}, // ease out cubic at 0.5
		{remoteScaleEase, 200},
		{2 * remoteScaleEase, 200},
	}
	for _, tc := range cases {
		if got := s.valueAt(start.Add(tc.after)); got != tc.want {
			t.Fatalf("scale after %s = %v, want %v", tc.after, got, tc.want)
		}
	}
	// A target that does not change starts no new ease.
	s.target(200, start.Add(3*remoteScaleEase))
	if got := s.valueAt(start.Add(3*remoteScaleEase + remoteScaleEase/2)); got != 200 {
		t.Fatalf("unchanged target moved the scale to %v", got)
	}
}

// TestRemoteScaleEasingInFrame draws the same data at two times and checks the
// curve rises as the scale eases from the previous peak to the new one.
func TestRemoteScaleEasingInFrame(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 100), nil)
	// The previous peak was twice the value, so the first frame draws the
	// curve at half height and the completed ease draws it at the top.
	c.Scales["billable"] = &remoteScale{from: 200, to: 200, start: c.Now, set: true}
	rowOf := func(ch remoteChart) int {
		b := tui.New(40, 10)
		drawRemoteChart(b, ch)
		for y := 0; y < b.Height(); y++ {
			for x := 0; x < b.Width(); x++ {
				if r := b.Cell(x, y).Rune; r >= 0x2800 && r <= 0x28ff {
					return y
				}
			}
		}
		return -1
	}
	early := rowOf(c)
	if early < 0 {
		t.Fatal("no curve drawn")
	}
	settled := c
	settled.Now = topBase.Add(remoteScaleEase)
	if got := rowOf(settled); got < 0 || got >= early {
		t.Fatalf("the curve did not rise as the scale eased: first row %d, settled row %d", early, got)
	}
}

// TestRemoteStyles checks the colours the braille dots and the fill carry.
func TestRemoteStyles(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 30, 100*1024, 0), nil)
	b := tui.New(40, 10)
	drawRemoteChart(b, c)
	green, dim := 0, 0
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			cell := b.Cell(x, y)
			if cell.Rune < 0x2800 || cell.Rune > 0x28ff {
				continue
			}
			if cell.Style.FG == remoteGreen.FG {
				green++
			}
			if cell.Style.Attrs&tui.AttrDim != 0 {
				dim++
			}
		}
	}
	if green == 0 {
		t.Fatal("no braille cell carries the curve colour")
	}
	if dim == 0 {
		t.Fatal("the fill under the curve carries no dim braille cell")
	}
}

// TestRemoteTwoSeries drops a second series of another colour into each half,
// which is the shape a packet curve arrives in.
func TestRemoteTwoSeries(t *testing.T) {
	c := remoteTestChart(nil, nil)
	c.Up = []remoteSeries{
		{Label: "billable", Style: remoteGreen, Group: "billable", Values: remoteSpike(60, 20, 72*1024, 900)},
		{Label: "pkt", Style: remotePink, Group: "billable", Values: remoteSpike(60, 20, 600, 20)},
	}
	b := tui.New(50, 10)
	drawRemoteChart(b, c)
	seen := map[string]bool{}
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			if cell := b.Cell(x, y); cell.Style.FG == remotePink.FG {
				seen["pink"] = true
			}
			if cell := b.Cell(x, y); cell.Style.FG == remoteGreen.FG {
				seen["green"] = true
			}
		}
	}
	if !seen["pink"] || !seen["green"] {
		t.Fatalf("two series drawn as one colour: %v", seen)
	}
	if got := strings.TrimRight(b.String(), "\n"); !strings.Contains(got, "billable pkt ▲") {
		t.Fatalf("legend names only one series:\n%s", got)
	}
}

// TestRemoteWithoutScaleState draws a chart whose caller holds no easing state:
// it must still be scaled to its window peak instead of collapsing to nothing.
func TestRemoteWithoutScaleState(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 100*1024), nil)
	c.Scales = nil
	b := tui.New(40, 10)
	drawRemoteChart(b, c)
	l := remoteLayout(10)
	for x := 0; x < b.Width(); x++ {
		if r := b.Cell(x, l.upY).Rune; r < 0x2800 || r > 0x28ff {
			t.Fatalf("the curve does not reach the top of its half without scale state:\n%s", b.String())
		}
	}
}

// TestRemoteSlidesOneDotAtATime advances the clock inside one sample and checks
// the curve moves left: the sub-column scroll comes from the clock, with no
// second timer and no extra state.
func TestRemoteSlidesOneDotAtATime(t *testing.T) {
	up := remoteSpike(60, 30, 100*1024, 0)
	c := remoteTestChart(up, nil)
	b := tui.New(40, 10)
	drawRemoteChart(b, c)
	first := b.String()
	c.Now = topBase.Add(250 * time.Millisecond)
	c.Newest = topBase // the newest sample keeps its time; the clock moves on
	b2 := tui.New(40, 10)
	drawRemoteChart(b2, c)
	second := b2.String()
	if first == second {
		t.Fatal("the curve did not move as the clock advanced")
	}
}

// TestRemotePeakLabelStaysInside checks the sliding label never leaves the
// plot, for a peak at either edge.
func TestRemotePeakLabelStaysInside(t *testing.T) {
	const w = 30
	for _, at := range []int{0, 1, 58, 59} {
		c := remoteTestChart(remoteSpike(60, at, 100*1024, 0), nil)
		b := tui.New(w, 10)
		drawRemoteChart(b, c)
		for y := 0; y < b.Height(); y++ {
			row := make([]rune, 0, w)
			for x := 0; x < b.Width(); x++ {
				row = append(row, b.Cell(x, y).Rune)
			}
			text := row
			for i := 0; i+6 < len(text); i++ {
				if string(text[i:i+6]) != "KiB/s " {
					continue
				}
				start := i - len("100.0")
				if start < 0 || i+7 > w {
					t.Fatalf("peak label runs off the %d wide plot at peak %d: %q", w, at, string(text))
				}
			}
		}
	}
}

// TestRemoteRenderProof writes the panel for a sample window as ANSI text, so
// the screenshot in the scratch directory is drawn by the same code the panel
// runs. It only runs when the variable is set, since it writes outside the
// repository.
func TestRemoteRenderProof(t *testing.T) {
	dir := os.Getenv("URNET_REMOTE_PROOF")
	if dir == "" {
		t.Skip("set URNET_REMOTE_PROOF to a directory to write the ANSI proof")
	}
	up := remoteFlat(60, 0)
	down := remoteFlat(60, 0)
	for i := 0; i < 60; i++ {
		up[i] = float64(400 + max(0, 40-absInt(i-25))*1800)
		down[i] = float64(2000 + max(0, 30-absInt(i-25))*14000)
	}
	c := remoteTestChart(up, down)
	b := tui.New(78, 11)
	drawRemoteChart(b, c)
	path := dir + "/remote_panel.ansi"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(remoteANSI(b)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/remote_panel.txt", []byte(b.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

// remoteANSI renders a buffer with SGR codes, so a screenshot tool can show the
// panel in the colours a terminal does.
func remoteANSI(b *tui.Buffer) string {
	var out strings.Builder
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y)
			if c.Continuation() {
				continue
			}
			code := remoteSGR(c.Style)
			if code == "" {
				out.WriteRune(c.Rune)
				continue
			}
			fmt.Fprintf(&out, "\x1b[%sm%c\x1b[0m", code, c.Rune)
		}
		out.WriteString("\n")
	}
	return out.String()
}

func remoteSGR(st tui.Style) string {
	var parts []string
	switch st.FG.Kind {
	case tui.Color16:
		if st.FG.Value < 8 {
			parts = append(parts, fmt.Sprintf("3%d", st.FG.Value))
		} else {
			parts = append(parts, fmt.Sprintf("9%d", st.FG.Value-8))
		}
	case tui.Color256:
		parts = append(parts, fmt.Sprintf("38;5;%d", st.FG.Value))
	case tui.ColorRGB:
		parts = append(parts, fmt.Sprintf("38;2;%d;%d;%d", st.FG.Value>>16&0xff, st.FG.Value>>8&0xff, st.FG.Value&0xff))
	}
	if st.Attrs&tui.AttrBold != 0 {
		parts = append(parts, "1")
	}
	if st.Attrs&tui.AttrDim != 0 {
		parts = append(parts, "2")
	}
	return strings.Join(parts, ";")
}
