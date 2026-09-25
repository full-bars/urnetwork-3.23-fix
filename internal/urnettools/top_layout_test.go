package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func modelWithProxies(t *testing.T, up, degraded, connecting, dead int) *topModel {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := topFlowing(t)
	s.Proxies = SnapshotProxies{Up: up, Degraded: degraded, Connecting: connecting, Dead: dead}
	m.apply(0, s, nil)
	return m
}

func TestTopProxiesPanelSizeFollowsThePool(t *testing.T) {
	cases := []struct {
		up, deg, con, dead int
		want               int
	}{
		{0, 0, 0, 0, topProxiesCompactRows}, // direct only: none at all
		{1, 0, 0, 0, topProxiesCompactRows}, // one proxy
		{1, 1, 1, 0, topProxiesCompactRows}, // the largest compact pool
		{1, 1, 1, 1, topProxiesRows},        // one more: bars
		{58, 2, 0, 0, topProxiesRows},
	}
	for _, c := range cases {
		m := modelWithProxies(t, c.up, c.deg, c.con, c.dead)
		if got := m.proxiesRows(); got != c.want {
			t.Errorf("pool %d/%d/%d/%d: %d rows, want %d", c.up, c.deg, c.con, c.dead, got, c.want)
		}
	}
	// No snapshot yet: the full size, so the layout does not jump when the first
	// answer arrives for a real pool.
	clock := &fakeClock{t: topBase}
	if got := newTestModel(tui.DefaultTheme(), topProviders(1), clock).proxiesRows(); got != topProxiesRows {
		t.Errorf("no data: %d rows", got)
	}
}

func TestTopEventsPanelSizeFollowsTheEvents(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	want := func(n, rows int) {
		t.Helper()
		m.events = nil
		for i := 0; i < n; i++ {
			m.addEvent(topBase, fmt.Sprintf("e%d", i), topInfo)
		}
		if got := m.eventsRows(); got != rows {
			t.Errorf("%d events: %d rows, want %d", n, got, rows)
		}
	}
	want(0, 3) // frame plus the "no events yet" line
	want(1, 3)
	want(3, 5)
	want(4, 6)
	want(40, 6) // capped: the newest four
}

func TestTopCompactProxiesLine(t *testing.T) {
	m := modelWithProxies(t, 1, 0, 0, 0)
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "1 up   0 degraded   0 connecting   0 dead") {
		t.Fatalf("compact pool line missing:\n%s", txt)
	}
	if strings.Contains(txt, "████████████████████████") && strings.Contains(txt, "connecting ░") {
		t.Fatalf("bars still drawn for a one proxy pool:\n%s", txt)
	}
	m = modelWithProxies(t, 0, 0, 0, 0)
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "no proxies: direct only") {
		t.Fatalf("direct-only line missing:\n%s", txt)
	}
}

// The rows a small pool and a short event list give back must go to the graphs,
// not to blank space.
func TestTopGraphsGrowIntoReclaimedRows(t *testing.T) {
	height := func(m *topModel) int {
		txt, _ := showOnSim(t, m, 120, 40)
		n, in := 0, false
		for _, l := range strings.Split(txt, "\n") {
			switch {
			case strings.HasPrefix(l, "╭ Billable"):
				in = true
			case in && strings.HasPrefix(l, "╰"):
				return n
			case in:
				n++
			}
		}
		t.Fatalf("no Billable graph:\n%s", txt)
		return 0
	}
	big := modelWithProxies(t, 58, 2, 0, 0)
	small := modelWithProxies(t, 1, 0, 0, 0)
	for _, m := range []*topModel{big, small} {
		m.events = nil
		m.addEvent(topBase, "only event", topInfo)
	}
	hb, hs := height(big), height(small)
	if hs != hb+3 && hs != hb+2 && hs != hb+4 {
		t.Fatalf("graph interior %d rows with bars, %d with the one-line pool; the freed 3 rows should go to the two graphs", hb, hs)
	}
}

func TestTopAdaptiveLayoutFitsEverySize(t *testing.T) {
	for _, pool := range [][4]int{{0, 0, 0, 0}, {1, 0, 0, 0}, {58, 2, 0, 0}} {
		for _, w := range []int{72, 100, 140} {
			for h := 20; h <= 44; h += 4 {
				t.Run(fmt.Sprintf("%v/%dx%d", pool, w, h), func(t *testing.T) {
					m := modelWithProxies(t, pool[0], pool[1], pool[2], pool[3])
					got, _ := showOnSim(t, m, w, h)
					lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
					if len(lines) > h {
						t.Fatalf("%d rows on a %d tall screen", len(lines), h)
					}
					for i, l := range lines {
						if lw := tui.StringWidth(l); lw > w {
							t.Fatalf("row %d is %d wide on %d", i, lw, w)
						}
					}
				})
			}
		}
	}
}

func TestTopDirectOnlyGoldenScreen(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := topIdle(t)
	s.Proxies = SnapshotProxies{}
	m.apply(0, s, nil)
	got, _ := showOnSim(t, m, 120, 40)
	assertGolden(t, "top_direct_only_120x40", got)
}

// The Resources box is sized from resourceRows and filled by drawResources; if
// the two disagree the box clips a figure or shows blank rows again.
func TestTopResourceRowsMatchWhatIsDrawn(t *testing.T) {
	limit, fds, fdLimit, rss := uint64(4<<30), 10, 1024, uint64(50<<20)
	cases := map[string]SnapshotResources{
		"everything":     {HeapInuseBytes: 1 << 30, MemLimitBytes: &limit, OpenFDs: &fds, FDLimit: &fdLimit, RSSBytes: &rss, Goroutines: 90},
		"no limits":      {HeapInuseBytes: 1 << 30, OpenFDs: &fds, RSSBytes: &rss, Goroutines: 90},
		"heap only":      {HeapInuseBytes: 1 << 30},
		"goroutines":     {Goroutines: 5},
		"nothing at all": {},
		"fds and rss":    {OpenFDs: &fds, RSSBytes: &rss},
	}
	for name, res := range cases {
		for _, withInternals := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/internals=%v", name, withInternals), func(t *testing.T) {
				clock := &fakeClock{t: topBase}
				m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
				s := topFlowing(t)
				s.Resources = res
				m.apply(0, s, nil)
				if withInternals {
					m.applyInternals(0, fakeInternals(1, 100, 1, 1), nil)
				}
				b := tui.New(40, 12)
				m.drawResources(b)
				drawn := 0
				for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
					if strings.TrimSpace(l) != "" {
						drawn++
					}
				}
				if want := m.resourceRows(); drawn != want && !(drawn == 0 && want == 1) {
					t.Fatalf("drew %d rows, resourceRows says %d:\n%s", drawn, want, b.String())
				}
			})
		}
	}
}
