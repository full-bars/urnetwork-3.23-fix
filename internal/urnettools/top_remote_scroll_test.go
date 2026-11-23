package urnettools

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// remoteFrameDrift replays a recorded series one second at a time, renders the
// Remote page after each reading and compares each frame to the one before it.
// A frame is the previous one scrolled left by a whole number of dot columns
// plus new data at the right edge. The result is the share of plotted dots, on
// average, that differ from the previous frame after the best such scroll
// (0 to 6 columns). A chart that scrolls without reshaping scores a few
// percent. One that re-samples its curve at a new phase every frame scores
// far higher.
func remoteFrameDrift(t *testing.T, replies []LiveTraffic, level, from int) float64 {
	t.Helper()
	clock := &fakeClock{t: time.Unix(0, replies[0].AtUnixNano)}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.interval = time.Second
	snap := packetsFixture(t)
	snap.Rate.HistoryBps = nil
	snap.Traffic.TotalHistoryBps = nil
	m.apply(0, snap, nil)
	m.winLevel = level
	m.page = pageByID("remote")
	var prev [][]bool
	var mismatch, dots float64
	for i := range replies {
		lt := replies[i]
		clock.t = time.Unix(0, lt.AtUnixNano)
		m.applyTraffic(m.gen, &lt, nil)
		if i < from {
			continue
		}
		b := tui.New(120, 40)
		renderFresh(m, b)
		box := regionRect(t, m, "box:remote")
		in := b.Sub(tui.Rect{X: box.X + 1, Y: box.Y + 1, W: box.W - 2, H: box.H - 2})
		l := remoteLayout(in.Width(), in.Height())
		g := remoteDotGrid(in, l.upY, l.upRows+1+l.downRows, tui.DefaultTheme().Dim)
		if prev != nil {
			best := 1 << 30
			for s := 0; s <= 6; s++ {
				mis := 0
				for y := range g {
					for x := 0; x < len(g[y])-s-1; x++ {
						p := false
						if x+s < len(prev[y]) {
							p = prev[y][x+s]
						}
						if p != g[y][x] {
							mis++
						}
					}
				}
				best = min(best, mis)
			}
			n := 0
			for y := range g {
				for x := range g[y] {
					if g[y][x] {
						n++
					}
				}
			}
			mismatch += float64(best)
			dots += float64(n)
		}
		prev = g
	}
	return 100 * mismatch / dots
}

// The Remote chart scrolls without reshaping at every zoom level.
func TestRemoteChartScrollsWithoutReshaping(t *testing.T) {
	replies := recordedReplies(t, "top_remote_recorded_660s.jsonl")
	for _, c := range []struct {
		name  string
		level int
		bound float64
	}{{"1m", 1, 8}, {"5m", 2, 8}, {"15m", 3, 8}} {
		got := remoteFrameDrift(t, replies, c.level, 400)
		t.Logf("%s: %.1f%% of the plotted dots change shape between frames", c.name, got)
		if got > c.bound {
			t.Errorf("%s: %.1f%% of the plotted dots change shape between frames, want at most %.0f%%", c.name, got, c.bound)
		}
	}
}
