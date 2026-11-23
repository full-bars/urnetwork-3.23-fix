package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// directionFixture is topFlowing with the per-direction block filled in. Each
// lane is a different fraction of the same series, so a page that draws the
// wrong lane cannot match the right picture.
func directionFixture(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := topFlowing(t)
	scale := func(f float64) []float64 {
		out := make([]float64, len(s.Rate.HistoryBps))
		for i, v := range s.Rate.HistoryBps {
			out[i] = v * f
		}
		return out
	}
	s.Traffic.Directions = &SnapshotTrafficDirections{
		BillableRx: SnapshotTrafficDirection{HistoryBps: scale(1)},
		BillableTx: SnapshotTrafficDirection{HistoryBps: scale(0.2)},
		TotalRx:    SnapshotTrafficDirection{HistoryBps: scale(1.4)},
		TotalTx:    SnapshotTrafficDirection{HistoryBps: scale(0.3)},
	}
	return s
}

// directionPageModel renders the page with this id at the given size.
func directionPageModel(t *testing.T, w, h int, id string) (*topModel, *tui.Buffer, string) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, directionFixture(t), nil)
	m.page = pageByID(id)
	if m.page < 0 {
		t.Fatalf("no page with id %q", id)
	}
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b, b.String()
}

// peakTail is the largest of the newest n samples, which is what the plot
// labels when the window holds fewer samples than it has dot columns.
func peakTail(vals []float64, n int) float64 {
	peak := 0.0
	for i := len(vals) - 1; i >= 0 && i >= len(vals)-n; i-- {
		if vals[i] > peak {
			peak = vals[i]
		}
	}
	return peak
}

// upload is tx and download is rx, and each page reads its own quantity. A
// mapping that swapped the two, or one page reading the other's lanes, fails.
func TestTopDirectionDataMapsUploadToTxAndDownloadToRx(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := directionFixture(t)
	m.apply(0, s, nil)
	d := s.Traffic.Directions

	cases := []struct {
		name           string
		billable       bool
		wantUp, wantDn []float64
	}{
		{"billable", true, d.BillableTx.HistoryBps, d.BillableRx.HistoryBps},
		{"total", false, d.TotalTx.HistoryBps, d.TotalRx.HistoryBps},
	}
	for _, c := range cases {
		up, down, _, _, _, _, ok := m.directionData(c.billable)
		if !ok {
			t.Fatalf("%s: direction data missing", c.name)
		}
		if len(up) != len(c.wantUp) || len(down) != len(c.wantDn) {
			t.Fatalf("%s: lengths %d/%d, want %d/%d", c.name, len(up), len(down), len(c.wantUp), len(c.wantDn))
		}
		for i := range up {
			if up[i] != c.wantUp[i] || down[i] != c.wantDn[i] {
				t.Fatalf("%s: sample %d = %v up / %v down, want %v / %v",
					c.name, i, up[i], down[i], c.wantUp[i], c.wantDn[i])
			}
		}
	}
}

// A provider that predates the block keeps the dashboard and the Remote page
// and never offers the direction pages.
func TestTopDirectionPagesHiddenForOldProvider(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)

	if m.directionAvailable() {
		t.Fatal("a snapshot without the block reports directions")
	}
	if _, _, _, _, _, _, ok := m.directionData(true); ok {
		t.Fatal("directionData reported data without the block")
	}
	if m.billablePageAvailable(tui.Rect{}) || m.totalPageAvailable(tui.Rect{}) {
		t.Fatal("the direction pages are offered without the block")
	}
	idx, _ := m.pageStrip(m.pageArea())
	if len(idx) != 2 || idx[0] != 0 || idx[1] != pageByID("remote") {
		t.Fatalf("strip offers %v, want the dashboard and Remote only", idx)
	}
}

// With the block present both pages join the strip in table order.
func TestTopDirectionPagesJoinTheStrip(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, directionFixture(t), nil)
	idx, _ := m.pageStrip(m.pageArea())
	want := []int{pageByID("overview"), pageByID("remote"), pageByID("billable"), pageByID("total")}
	if len(idx) != len(want) {
		t.Fatalf("strip offers %v, want %v", idx, want)
	}
	for i := range want {
		if idx[i] != want[i] {
			t.Fatalf("strip offers %v, want %v", idx, want)
		}
	}
}

// The pages label upload above and download below, print the corner averages
// and label the window peak of each direction.
func TestTopDirectionPageFigures(t *testing.T) {
	for _, id := range []string{"billable", "total"} {
		m, _, text := directionPageModel(t, 120, 40, id)
		up, down, _, _, _, _, ok := m.directionData(id == "billable")
		if !ok {
			t.Fatalf("%s: direction data missing", id)
		}
		for _, want := range []string{"upload", "download", "▲", "▼"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s page is missing %q:\n%s", id, want, text)
			}
		}
		for _, want := range []string{
			tui.Rate(remoteAverage(up)) + "  ▲",
			tui.Rate(remoteAverage(down)) + "  ▼",
			tui.Rate(peakTail(up, 60)) + " ▲",
			tui.Rate(peakTail(down, 60)) + " ▼",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s page is missing the figure %q:\n%s", id, want, text)
			}
		}
		// The dashboard's panels stay off the page.
		for _, unwanted := range []string{"Proxies", "Events", "Resources"} {
			if strings.Contains(text, unwanted) {
				t.Fatalf("%s page drew the dashboard's %s panel:\n%s", id, unwanted, text)
			}
		}
		if regionByName(m.regions, "box:"+id) == nil {
			t.Fatalf("%s page recorded no hit region", id)
		}
	}
}

// A new provider sends the block before its first sample. The page then shows
// its own waiting line rather than the Remote chart's.
func TestTopDirectionPageBeforeTheFirstSample(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := topFlowing(t)
	s.Traffic.Directions = &SnapshotTrafficDirections{}
	m.apply(0, s, nil)
	m.page = pageByID("billable")
	b := tui.New(80, 24)
	renderFresh(m, b)
	if !strings.Contains(b.String(), "waiting for the first direction samples") {
		t.Fatalf("the page has no waiting line:\n%s", b.String())
	}
}

// The pages at a wide, a narrow and the smallest full-layout size. The goldens
// catch a curve that drifts, a label that moves or a strip that changes.
func TestTopDirectionPageGoldens(t *testing.T) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"120x40", 120, 40},
		{"80x24", 80, 24},
		{"72x20", 72, 20},
	}
	for _, id := range []string{"billable", "total"} {
		for _, sz := range sizes {
			_, _, text := directionPageModel(t, sz.w, sz.h, id)
			assertGolden(t, "top_page_"+id+"_"+sz.name, text)
		}
	}
}
