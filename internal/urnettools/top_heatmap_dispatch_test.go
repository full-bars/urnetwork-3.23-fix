package urnettools

import (
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

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
