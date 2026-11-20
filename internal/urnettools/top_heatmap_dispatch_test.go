package urnettools

import (
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func TestHeatmapTimelineDraws(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapTimeline
		got, glow := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyHeatmap(got)
		m.drawProxyGrid(glow)
		if got.String() == glow.String() {
			t.Fatalf("n=%d: Timeline must differ from Glow", n)
		}
	}
}

func TestHeatmapBlocksDraws(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapBlocks
		got, glow := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyHeatmap(got)
		m.drawProxyGrid(glow)
		if got.String() == glow.String() {
			t.Fatalf("n=%d: Blocks must differ from Glow", n)
		}
	}
}

func TestHeatmapLocalityDraws(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapLocality
		got, glow := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyHeatmap(got)
		m.drawProxyGrid(glow)
		if got.String() == glow.String() {
			t.Fatalf("n=%d: Locality must differ from Glow", n)
		}
	}
}

func TestHeatmapSonarDraws(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapSonar
		got, glow := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyHeatmap(got)
		m.drawProxyGrid(glow)
		if got.String() == glow.String() {
			t.Fatalf("n=%d: Sonar must differ from Glow", n)
		}
	}
}

func TestHeatmapAuroraDraws(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapAurora
		got, glow := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyHeatmap(got)
		m.drawProxyGrid(glow)
		if got.String() == glow.String() {
			t.Fatalf("n=%d: Aurora must differ from Glow", n)
		}
	}
}

func TestHeatmapGlowMatchesGrid(t *testing.T) {
	for _, n := range []int{1, 3, 50, 1001, 4000, 20000} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		want, got := tui.New(80, 20), tui.New(80, 20)
		m.drawProxyGrid(want)
		m.drawProxyHeatmap(got)
		for y := 0; y < got.Height(); y++ {
			for x := 0; x < got.Width(); x++ {
				if got.Cell(x, y) != want.Cell(x, y) {
					t.Fatalf("n=%d at %d,%d: got %+v, want %+v", n, x, y, got.Cell(x, y), want.Cell(x, y))
				}
			}
		}
	}
}
