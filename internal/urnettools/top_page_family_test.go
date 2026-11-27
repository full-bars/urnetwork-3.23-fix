package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// familyFixture is topFlowing with the IP family split filled in. Each family is
// a fixed fraction of the same series and each quantity a different one, so a
// page that reads the wrong lane or swaps the two families cannot match the
// right picture.
func familyFixture(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := topFlowing(t)
	scale := func(f float64) []float64 {
		out := make([]float64, len(s.Rate.HistoryBps))
		for i, v := range s.Rate.HistoryBps {
			out[i] = v * f
		}
		return out
	}
	direction := func(f float64) SnapshotFamilySplit {
		return SnapshotFamilySplit{
			V4: SnapshotTrafficDirection{HistoryBps: scale(f * 0.78)},
			V6: SnapshotTrafficDirection{HistoryBps: scale(f * 0.22)},
		}
	}
	s.Traffic.Directions = &SnapshotTrafficDirections{
		BillableRx: SnapshotTrafficDirection{HistoryBps: scale(1)},
		BillableTx: SnapshotTrafficDirection{HistoryBps: scale(0.2)},
		TotalRx:    SnapshotTrafficDirection{HistoryBps: scale(1.4)},
		TotalTx:    SnapshotTrafficDirection{HistoryBps: scale(0.3)},
		Families: &SnapshotTrafficFamilies{
			BillableRx: direction(1),
			BillableTx: direction(0.2),
			TotalRx:    direction(1.4),
			TotalTx:    direction(0.3),
		},
	}
	return s
}

// familyPageModel renders the IP family page at the given size with this theme.
func familyPageModel(t *testing.T, w, h int, th tui.Theme) (*topModel, *tui.Buffer, string) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(th, topProviders(1), clock)
	m.apply(0, familyFixture(t), nil)
	m.page = pageByID(familyPageID)
	if m.page < 0 {
		t.Fatalf("no page with id %q", familyPageID)
	}
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b, b.String()
}

// The page reads the billable family lanes: upload is tx above the centre line,
// download is rx below it, and the two families stay apart.
func TestTopFamilyDataMapsUploadToTxAndFamilies(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := familyFixture(t)
	m.apply(0, s, nil)

	v4Up, v6Up, v4Down, v6Down, _, _, ok := m.familyData()
	if !ok {
		t.Fatal("family data missing")
	}
	f := s.Traffic.Directions.Families
	for _, c := range []struct {
		name string
		got  []float64
		want []float64
	}{
		{"v4 upload", v4Up, f.BillableTx.V4.HistoryBps},
		{"v6 upload", v6Up, f.BillableTx.V6.HistoryBps},
		{"v4 download", v4Down, f.BillableRx.V4.HistoryBps},
		{"v6 download", v6Down, f.BillableRx.V6.HistoryBps},
	} {
		if len(c.got) != len(c.want) {
			t.Fatalf("%s: %d samples, want %d", c.name, len(c.got), len(c.want))
		}
		for i := range c.got {
			if c.got[i] != c.want[i] {
				t.Fatalf("%s: sample %d = %v, want %v", c.name, i, c.got[i], c.want[i])
			}
		}
	}
}

// A provider that predates the family block keeps the pages it always had and
// never offers the IP family page.
func TestTopFamilyPageHiddenForOldProvider(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, directionFixture(t), nil)

	if m.familyAvailable() {
		t.Fatal("a snapshot without the family block reports a split")
	}
	if _, _, _, _, _, _, ok := m.familyData(); ok {
		t.Fatal("familyData reported data without the block")
	}
	if m.familyPageAvailable(tui.Rect{}) {
		t.Fatal("the family page is offered without the block")
	}
	for _, i := range func() []int { idx, _ := m.pageStrip(m.pageArea()); return idx }() {
		if topPages[i].id == familyPageID {
			t.Fatal("the strip offers the family page without the block")
		}
	}
}

// With the block present the page joins the strip last, in table order.
func TestTopFamilyPageJoinsTheStrip(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, familyFixture(t), nil)
	idx, _ := m.pageStrip(m.pageArea())
	want := []int{pageByID("overview"), pageByID("remote"), pageByID("billable"), pageByID("total"), pageByID(familyPageID)}
	if len(idx) != len(want) {
		t.Fatalf("strip offers %v, want %v", idx, want)
	}
	for i := range want {
		if idx[i] != want[i] {
			t.Fatalf("strip offers %v, want %v", idx, want)
		}
	}
}

// Both pages remain available when the provider reports transports and families.
func TestTopFamilyPageAlongsideTransports(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, familyFixture(t), nil)
	prev, _ := mixedWindow()
	m.transportsHistory.push(transportTestBase, prev)
	idx, _ := m.pageStrip(m.pageArea())
	if len(idx) != 6 || topPages[idx[4]].id != "transports" || topPages[idx[5]].id != familyPageID {
		t.Fatalf("unexpected pages: %v", idx)
	}
	m.selectPageID(familyPageID)
	if m.selectedPage().id != familyPageID {
		t.Fatal("family page cannot be selected")
	}
}

// The share is whole percents that always sum to 100, IPv6 taking the
// remainder. An idle window has no share to state and says so.
func TestTopFamilyShare(t *testing.T) {
	cases := []struct {
		name           string
		v4, v6         float64
		wantV4, wantV6 int
		wantIdle       bool
	}{
		{"even", 100, 100, 50, 50, false},
		{"mostly v4", 780, 220, 78, 22, false},
		{"all v4", 500, 0, 100, 0, false},
		{"all v6", 0, 500, 0, 100, false},
		{"rounds down", 200, 100, 67, 33, false},
		{"rounds up", 100, 200, 33, 67, false},
		{"rounds to zero", 1, 999, 0, 100, false},
		{"idle", 0, 0, 0, 0, true},
		{"negative samples are not traffic", -50, 0, 0, 0, true},
	}
	for _, c := range cases {
		v4pct, v6pct, idle := familyShare(c.v4, c.v6)
		if v4pct != c.wantV4 || v6pct != c.wantV6 || idle != c.wantIdle {
			t.Fatalf("%s: share(%v, %v) = %d/%d idle=%v, want %d/%d idle=%v",
				c.name, c.v4, c.v6, v4pct, v6pct, idle, c.wantV4, c.wantV6, c.wantIdle)
		}
		if !idle && v4pct+v6pct != 100 {
			t.Fatalf("%s: percentages sum to %d", c.name, v4pct+v6pct)
		}
	}

	// Every ratio in a sweep sums to 100 too.
	for v6 := 0; v6 <= 100; v6++ {
		v4pct, v6pct, idle := familyShare(float64(100-v6), float64(v6))
		if idle || v4pct+v6pct != 100 || v4pct < 0 || v6pct < 0 {
			t.Fatalf("share(100-%d, %d) = %d/%d idle=%v", v6, v6, v4pct, v6pct, idle)
		}
	}
}

// The page labels both families on both halves, prints the corner averages and
// peaks, states the window share, and draws none of the dashboard's panels.
func TestTopFamilyPageFigures(t *testing.T) {
	m, _, text := familyPageModel(t, 120, 40, tui.DefaultTheme())
	for _, want := range []string{"IPv4 / IPv6", "IPv4", "IPv6", "▲", "▼"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the page is missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "IPv4 78%") || !strings.Contains(text, "IPv6 22%") {
		t.Fatalf("the share bar is missing from the page:\n%s", text)
	}
	v4Up, v6Up, v4Down, v6Down, _, _, ok := m.familyData()
	if !ok {
		t.Fatal("family data missing")
	}
	for _, want := range []string{
		tui.Rate(remoteAverage(v4Up)) + "  " + tui.Rate(remoteAverage(v6Up)) + "  ▲",
		tui.Rate(remoteAverage(v4Down)) + "  " + tui.Rate(remoteAverage(v6Down)) + "  ▼",
		tui.Rate(peakTail(v4Up, 60)) + " " + tui.Rate(peakTail(v6Up, 60)) + " ▲",
		tui.Rate(peakTail(v4Down, 60)) + " " + tui.Rate(peakTail(v6Down, 60)) + " ▼",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the page is missing the figure %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Proxies", "Events", "Resources"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("the page drew the dashboard's %s panel:\n%s", unwanted, text)
		}
	}
	if regionByName(m.regions, "box:"+familyPageID) == nil {
		t.Fatal("the page recorded no hit region")
	}
}

// An idle window states zero and says idle instead of dividing by nothing.
func TestTopFamilyPageIdleWindow(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := familyFixture(t)
	f := s.Traffic.Directions.Families
	for _, split := range []*SnapshotFamilySplit{&f.BillableRx, &f.BillableTx, &f.TotalRx, &f.TotalTx} {
		split.V4 = SnapshotTrafficDirection{HistoryBps: make([]float64, len(split.V4.HistoryBps))}
		split.V6 = SnapshotTrafficDirection{HistoryBps: make([]float64, len(split.V6.HistoryBps))}
	}
	m.apply(0, s, nil)
	m.page = pageByID(familyPageID)
	b := tui.New(80, 24)
	renderFresh(m, b)
	text := b.String()
	if !strings.Contains(text, "IPv4 0%") || !strings.Contains(text, "IPv6 0%") || !strings.Contains(text, "idle") {
		t.Fatalf("idle window has no idle line:\n%s", text)
	}
}

// A new provider sends the block before its first sample. The page then says so
// rather than drawing an empty chart.
func TestTopFamilyPageBeforeTheFirstSample(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := topFlowing(t)
	s.Traffic.Directions = &SnapshotTrafficDirections{Families: &SnapshotTrafficFamilies{}}
	m.apply(0, s, nil)
	m.page = pageByID(familyPageID)
	b := tui.New(80, 24)
	renderFresh(m, b)
	if !strings.Contains(b.String(), "waiting for the first IP family samples") {
		t.Fatalf("the page has no waiting line:\n%s", b.String())
	}
}

// A sample that went backwards is skipped by the sampler, so a lane goes flat
// from there on. The page draws the flat lane rather than a spike, and the
// share is the arithmetic on the same histories it drew.
func TestTopFamilyPageAfterADecrease(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := familyFixture(t)
	// The IPv6 billable download lane loses its second half, which is what a
	// counter that went backwards leaves behind: zeroes, never negative rates.
	rx := s.Traffic.Directions.Families.BillableRx.V6.HistoryBps
	for i := len(rx) / 2; i < len(rx); i++ {
		rx[i] = 0
	}
	m.apply(0, s, nil)
	m.page = pageByID(familyPageID)
	b := tui.New(120, 40)
	renderFresh(m, b)
	text := b.String()
	// The share bar states the split of the window on screen, not the whole
	// history: the fixture's 22 percent no longer describes the visible part
	// once half the IPv6 download lane is flat.
	f := s.Traffic.Directions.Families
	_, _, _, _, newest, interval, _ := m.familyData()
	want := familyShareTextWindow(f.BillableTx.V4.HistoryBps, f.BillableTx.V6.HistoryBps,
		f.BillableRx.V4.HistoryBps, f.BillableRx.V6.HistoryBps, newest, m.now(), interval, remoteDefaultWindow)
	if !strings.Contains(text, want) {
		t.Fatalf("the share bar does not state %q:\n%s", want, text)
	}
}

func TestFamilyShareUsesChartWindow(t *testing.T) {
	// 600 one-second samples: IPv4 fills all but the newest minute, IPv6 fills
	// the minute the chart shows. The whole-history share is 90/10; the share
	// bar must state the visible 0/100.
	v4 := make([]float64, 600)
	for i := 0; i < 539; i++ {
		v4[i] = 1
	}
	v6 := make([]float64, 600)
	for i := 539; i < 600; i++ {
		v6[i] = 1
	}
	now := topBase.Add(600 * time.Second)
	got := familyShareTextWindow(v4, v6, nil, nil, now, now, time.Second, remoteDefaultWindow)
	if got != "IPv4 0%  IPv6 100%" {
		t.Fatalf("share bar = %q, want the visible window's 0/100", got)
	}
}

// The page at a wide, a narrow and the smallest full-layout size, plus the mono
// fallback. The goldens catch a curve that drifts, a label that moves, a share
// that changes or a series that takes the other's colour.
func TestTopFamilyPageGoldens(t *testing.T) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"120x40", 120, 40},
		{"80x24", 80, 24},
		{"72x20", 72, 20},
	}
	for _, sz := range sizes {
		_, _, text := familyPageModel(t, sz.w, sz.h, tui.DefaultTheme())
		assertGolden(t, "top_page_family_"+sz.name, text)
	}
	_, _, mono := familyPageModel(t, 80, 24, tui.MonoTheme())
	assertGolden(t, "top_page_family_mono_80x24", mono)
	_, _, colorblind := familyPageModel(t, 80, 24, mustTheme(t, "colorblind"))
	assertGolden(t, "top_page_family_colorblind_80x24", colorblind)
}

// The ASCII fallback tells the two families apart by glyph, because a theme with
// no colour has nothing else to separate them with.
func TestTopFamilyPageASCIIGlyphs(t *testing.T) {
	_, _, text := familyPageModel(t, 120, 40, tui.MonoTheme())
	if !strings.Contains(text, "#") || !strings.Contains(text, "+") {
		t.Fatalf("the ASCII page does not draw both series glyphs:\n%s", text)
	}
	for _, want := range []string{"IPv4", "IPv6", "▲", "▼"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the ASCII page is missing %q:\n%s", want, text)
		}
	}
}
