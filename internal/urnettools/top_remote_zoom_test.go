package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// recordedReplies loads a recorded series of traffic replies from a production
// provider: the cumulative counters only, one reply a second.
func recordedReplies(t *testing.T, name string) []LiveTraffic {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []LiveTraffic
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r struct {
			Traffic LiveTraffic `json:"traffic"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r.Traffic)
	}
	return out
}

// replayRemote feeds the replies into a model the way top's poll does and
// renders page 2 at the zoom level.
func replayRemote(t *testing.T, replies []LiveTraffic, level, w, h int) (*topModel, *tui.Buffer) {
	t.Helper()
	clock := &fakeClock{t: time.Unix(0, replies[0].AtUnixNano)}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.interval = time.Second
	snap := packetsFixture(t)
	// The ring fills from the replies alone, with no made-up history.
	snap.Rate.HistoryBps = nil
	snap.Traffic.TotalHistoryBps = nil
	m.apply(0, snap, nil)
	for i := range replies {
		lt := replies[i]
		clock.t = time.Unix(0, lt.AtUnixNano)
		m.applyTraffic(m.gen, &lt, nil)
	}
	m.winLevel = level
	m.page = pageByID("remote")
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b
}

// jumpColumns counts the dot columns of the plot where a curve runs straight
// up or down for more than eight dots, two text rows: the wall of hanging
// bars a noisy one second series draws when every sample reaches the plot.
// The curve runs through its samples as the app's does, so a real burst is a
// steep peak. Only a plot full of them is a wall.
func jumpColumns(t *testing.T, m *topModel, b *tui.Buffer) int {
	t.Helper()
	box := regionRect(t, m, "box:remote")
	in := b.Sub(tui.Rect{X: box.X + 1, Y: box.Y + 1, W: box.W - 2, H: box.H - 2})
	l := remoteLayout(in.Width(), in.Height())
	grid := remoteDotGrid(in, l.upY, l.upRows+1+l.downRows, tui.DefaultTheme().Dim)
	n := 0
	for x := range grid[0] {
		run, worst := 0, 0
		for y := range grid {
			if grid[y][x] {
				run++
				worst = max(worst, run)
			} else {
				run = 0
			}
		}
		if worst > 8 {
			n++
		}
	}
	return n
}

// At 5m a noisy one second series is averaged into buckets a few dot columns
// wide, so it reads as a smooth curve instead of a wall of bars.
func TestRemoteNoisySeriesIsSmoothAtEveryZoom(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_660s.jsonl")
	short := recordedReplies(t, "top_remote_recorded_150s.jsonl")
	for _, c := range []struct {
		name    string
		replies []LiveTraffic
		level   int
		bound   int
	}{
		{"10s", short, 0, 4},
		{"1m", short, 1, 25},
		{"5m", long, 2, 25},
		{"15m", long, 3, 25},
	} {
		m, b := replayRemote(t, c.replies, c.level, 120, 40)
		if n := jumpColumns(t, m, b); n > c.bound {
			t.Errorf("%s: %d dot columns hang in runs of more than eight dots, want at most %d:\n%s", c.name, n, c.bound, b.String())
		}
	}
}

// Page 2 at every zoom level over the recorded series, at a wide and a
// standard size.
func TestRemoteZoomGoldens(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_660s.jsonl")
	short := recordedReplies(t, "top_remote_recorded_150s.jsonl")
	for level, name := range []string{"10s", "1m", "5m", "15m"} {
		replies := short
		if level >= 2 {
			replies = long
		}
		for _, size := range [][2]int{{120, 40}, {80, 24}} {
			_, b := replayRemote(t, replies, level, size[0], size[1])
			assertGolden(t, fmt.Sprintf("top_remote_zoom_%s_%dx%d", name, size[0], size[1]), b.String())
		}
	}
}

// graphDataColumns counts the columns of a graph box that carry any plotted
// cell: braille or block glyphs right of the axis.
func graphDataColumns(b *tui.Buffer, r tui.Rect) int {
	n := 0
	for x := r.X + 1; x < r.X+r.W-1; x++ {
		for y := r.Y + 1; y < r.Y+r.H-1; y++ {
			c := b.Cell(x, y).Rune
			if (c > 0x2800 && c <= 0x28ff) || (c >= 0x2581 && c <= 0x2588) {
				n++
				break
			}
		}
	}
	return n
}

// The Billable and Total graphs of page 1 fill their plot at every zoom level
// over the recorded series. The window series is placed on its own time axis,
// so it must not be bucketed again on an absolute time index: a nanosecond
// timestamp as that index lost its precision in float64 and folded the whole
// window into one column.
func TestTopWindowGraphsFillThePlot(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_660s.jsonl")
	short := recordedReplies(t, "top_remote_recorded_150s.jsonl")
	for level, name := range []string{"10s", "1m", "5m", "15m"} {
		replies := short
		if level >= 2 {
			replies = long
		}
		for _, size := range [][2]int{{120, 40}, {80, 24}} {
			m, _ := replayRemote(t, replies, level, size[0], size[1])
			m.page = 0
			b := tui.New(size[0], size[1])
			renderFresh(m, b)
			// The graph band is one region. A second graph starts at its own
			// title row, so the band is split there.
			var boxes []tui.Rect
			for _, r := range m.regions {
				if r.Name == "box:graph" {
					boxes = append(boxes, r.Rect)
				}
			}
			for _, r := range m.regions {
				if r.Name == "title:graph" && len(boxes) == 1 && r.Rect.Y > boxes[0].Y {
					band := boxes[0]
					boxes = []tui.Rect{
						{X: band.X, Y: band.Y, W: band.W, H: r.Rect.Y - band.Y},
						{X: band.X, Y: r.Rect.Y, W: band.W, H: band.Y + band.H - r.Rect.Y},
					}
				}
			}
			if len(boxes) == 0 {
				t.Fatalf("%s %dx%d: no graph on page 1:\n%s", name, size[0], size[1], b.String())
			}
			for _, r := range boxes {
				// The data covers the whole window at 10s and 1m, and the
				// newest 150 or 330 seconds at 5m and 15m.
				want := (r.W - 12) / 3
				if level == 3 {
					want = (r.W - 12) / 4
				}
				if got := graphDataColumns(b, r); got < want {
					t.Errorf("%s %dx%d: a graph draws data in %d columns, want at least %d:\n%s", name, size[0], size[1], got, want, b.String())
				}
			}
		}
	}
}

// Pages 2, 3 and 4 are one family over the recorded series: lines only, no
// fill cells, and the same flat plot height at the same size.
func TestMirroredPagesAreOneFamily(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_660s.jsonl")
	for _, size := range [][2]int{{120, 40}, {80, 24}} {
		spans := map[string]int{}
		for _, page := range []string{"remote", "billable", "total"} {
			m, _ := replayRemote(t, long, 1, size[0], size[1])
			m.page = pageByID(page)
			b := tui.New(size[0], size[1])
			renderFresh(m, b)
			first, last, fill := -1, -1, 0
			for y := 0; y < b.Height(); y++ {
				for x := 0; x < b.Width(); x++ {
					c := b.Cell(x, y)
					if c.Rune <= 0x2800 || c.Rune > 0x28ff {
						continue
					}
					if c.Style.Attrs&tui.AttrDim != 0 {
						fill++
					}
					if first < 0 {
						first = y
					}
					last = y
				}
			}
			if fill != 0 {
				t.Errorf("%s %dx%d: %d dim fill cells, want lines only:\n%s", page, size[0], size[1], fill, b.String())
			}
			spans[page] = last - first
		}
		if spans["billable"] > spans["remote"] || spans["total"] > spans["remote"] {
			t.Errorf("%dx%d: the plots span %v rows, want pages 3 and 4 no taller than page 2", size[0], size[1], spans)
		}
	}
}
