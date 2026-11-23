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
// up or down for more than four dots: the hanging bars a noisy one second
// series draws when every sample reaches the plot.
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
		if worst > 4 {
			n++
		}
	}
	return n
}

// At 5m a noisy one second series is averaged into buckets a few dot columns
// wide, so it reads as a smooth curve instead of a wall of bars.
func TestRemoteNoisySeriesIsSmoothAtEveryZoom(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_330s.jsonl")
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
			t.Errorf("%s: %d dot columns hang in runs of more than four dots, want at most %d:\n%s", c.name, n, c.bound, b.String())
		}
	}
}

// Page 2 at every zoom level over the recorded series, at a wide and a
// standard size.
func TestRemoteZoomGoldens(t *testing.T) {
	long := recordedReplies(t, "top_remote_recorded_330s.jsonl")
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
