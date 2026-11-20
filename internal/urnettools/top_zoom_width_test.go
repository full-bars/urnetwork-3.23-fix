package urnettools

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

func TestZoomUsesPlotWidthAndUnzoom(t *testing.T) {
	for _, theme := range []string{"default", "mono", "colorblind"} {
		for _, width := range []int{30, 100} {
			t.Run(fmt.Sprintf("%s/%d", theme, width), func(t *testing.T) {
				m, _ := liveModel(t, time.Second)
				m.theme, _ = tui.ThemeByName(theme)
				for i := 1; i <= 15; i++ {
					m.zoom.push(int64(i)*int64(time.Second), 1000, 2000)
				}
				m.zoomOn = true
				sr := m.series()
				b := tui.New(width, 4)
				m.drawGraph(b, sr, sr.billable, sr.scaleB, 0, m.theme.OK)
				if b.Cell(10, 3).Rune == 0 || b.Cell(10, 3).Rune == ' ' {
					t.Fatal("15 seconds of zoom history should fill the plot")
				}
				m.zoomOn = false
				if m.series().zoom {
					t.Fatal("unzoom should restore normal history")
				}
			})
		}
	}
}
