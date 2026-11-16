package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func TestTopDiagnosticPanels(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	for _, tc := range []struct {
		key   rune
		label string
	}{{'a', "Alerts"}, {'i', "Resource gauges"}, {'v', "Extender carriers"}} {
		m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: tc.key})
		b := tui.New(100, 30)
		m.render(b)
		if !strings.Contains(b.String(), tc.label) {
			t.Fatalf("%c did not display %s", tc.key, tc.label)
		}
		m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
		if m.panel != "" {
			t.Fatalf("%c: Escape did not close panel", tc.key)
		}
	}
}

func TestTopAlertsPrioritizeFailures(t *testing.T) {
	m := panelGoldenState(t)
	m.extender.RoleState = "error"
	b := tui.New(80, 1)
	m.drawAlertBanner(b)
	if !strings.Contains(b.String(), "Extender refusing") {
		t.Fatalf("severe alert hidden: %s", b.String())
	}
}

func TestTopMissingThreadCountIsNotZero(t *testing.T) {
	m := panelGoldenState(t)
	limit := uint64(512)
	m.snap.Resources.Threads, m.snap.Resources.TasksMax = nil, &limit
	b := tui.New(80, 5)
	m.drawGauges(b)
	if strings.Contains(b.String(), "threads") {
		t.Fatalf("missing threads shown: %s", b.String())
	}
}
