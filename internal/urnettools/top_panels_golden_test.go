package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// panelGoldenState builds a model carrying all the data the panels read.
func panelGoldenState(t *testing.T) *topModel {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	fd, limit := 71, 100
	ssMem := uint64(256)
	m.apply(0, &NodeSnapshot{
		State:          "degraded",
		RestartPending: true,
		Resources:      SnapshotResources{OpenFDs: &fd, FDLimit: &limit, MemLimitBytes: &ssMem, HeapInuseBytes: 31, Goroutines: 42},
		Proxies:        SnapshotProxies{Up: 4, Configured: 10},
	}, nil)
	m.extender = ExtenderReading{Source: SourceControlSocket, Known: true, Setting: "on", Running: true,
		Carriers: []ExtenderCarrierStats{{Carrier: "attwifi", Served: 1024, Refused: 7, RefusalsByStage: map[string]uint64{"auth": 3, "throttle": 4}}},
		Families: []ExtenderFamily{{IpVersion: 4, State: "activated"}, {IpVersion: 6, State: "held"}},
	}
	m.rt.cur = &NodeInternals{Goroutines: 42, HeapGoalBytes: 128 * 1024 * 1024, HeapObjectsBytes: 96 * 1024 * 1024}
	return m
}

func drawPanel(t *testing.T, m *topModel, w, h int, kind string) string {
	t.Helper()
	b := tui.New(w, h)
	switch kind {
	case "alerts":
		m.drawAlertBanner(b)
	case "gauges":
		m.drawGauges(b)
	case "carriers":
		m.drawCarriers(b)
	default:
		t.Fatalf("unknown panel %q", kind)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func TestTopPanelsGolden(t *testing.T) {
	for _, tt := range []struct {
		name string
		w, h int
		kind string
	}{
		{"top_alerts_golden_80x24", 80, 24, "alerts"},
		{"top_alerts_golden_120x40", 120, 40, "alerts"},
		{"top_alerts_golden_150x40", 150, 40, "alerts"},
		{"top_gauges_golden_80x24", 80, 24, "gauges"},
		{"top_gauges_golden_120x40", 120, 40, "gauges"},
		{"top_gauges_golden_150x40", 150, 40, "gauges"},
		{"top_carriers_golden_80x24", 80, 24, "carriers"},
		{"top_carriers_golden_120x40", 120, 40, "carriers"},
		{"top_carriers_golden_150x40", 150, 40, "carriers"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := panelGoldenState(t)
			got := drawPanel(t, m, tt.w, tt.h, tt.kind)
			assertGolden(t, tt.name, got)
		})
	}
}
