package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func topHeatGridModelWithStyle(t *testing.T, th tui.Theme, n int, s heatmapStyle) *topModel {
	m := topHeatGridModel(t, th, n)
	m.proxyHeatmapStyle = s
	return m
}

func TestHeatmapStyleGoldenDefault(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{100, 30}, {120, 40}} {
		for _, s := range heatmapStyles {
			name := fmt.Sprintf("top_heatmap_%s_default_%dx%d", strings.ToLower(s.String()), sz.w, sz.h)
			m := topHeatGridModelWithStyle(t, tui.DefaultTheme(), 1001, s)
			got, _ := showOnSim(t, m, sz.w, sz.h)
			assertGolden(t, name, got)
		}
	}
}

func TestHeatmapStyleGoldenMono(t *testing.T) {
	for _, s := range heatmapStyles {
		name := fmt.Sprintf("top_heatmap_%s_mono_100x30", strings.ToLower(s.String()))
		m := topHeatGridModelWithStyle(t, tui.MonoTheme(), 1001, s)
		got, _ := showOnSim(t, m, 100, 30)
		assertGolden(t, name, got)
	}
}

func TestHeatmapStyleGoldenColorblind(t *testing.T) {
	th, ok := tui.ThemeByName("colorblind")
	if !ok {
		t.Fatal("colorblind theme is missing")
	}
	for _, s := range heatmapStyles {
		name := fmt.Sprintf("top_heatmap_%s_colorblind_100x30", strings.ToLower(s.String()))
		m := topHeatGridModelWithStyle(t, th, 1001, s)
		got, _ := showOnSim(t, m, 100, 30)
		assertGolden(t, name, got)
	}
}

// A panel too small for the grid falls back to the classic state bars, exactly
// as it does without a style chosen.
func TestHeatmapStyleFallsBackToBars(t *testing.T) {
	for _, s := range heatmapStyles {
		m := topHeatGridModelWithStyle(t, tui.DefaultTheme(), 1001, s)
		got, _ := showOnSim(t, m, 20, 6)
		assertGolden(t, fmt.Sprintf("top_heatmap_%s_bars_20x6", strings.ToLower(s.String())), got)
	}
}
