package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func pressRune(m *topModel, r rune) {
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: r})
}

// With no room for the Internals panel, g opens the goroutine list as an
// overlay. It used to flip a hidden switch and show nothing.
func TestGoroutineKeyOpensAnOverlayWhenThePanelHasNoRoom(t *testing.T) {
	m, _ := liveModel(t, 1e9)
	m.rt.ok, m.rt.gOK, m.withInternals = true, true, false
	m.rt.groups = &GoroutineGroups{Total: 12, Groups: []GoroutineGroup{{Count: 7, Func: "net.(*conn).Read"}, {Count: 5, Func: "time.Sleep"}}}
	pressRune(m, 'g')
	if m.panel != "goroutines" || !m.rt.showing {
		t.Fatalf("after g: panel %q showing %v", m.panel, m.rt.showing)
	}
	b := tui.New(120, 40)
	m.render(b)
	var text strings.Builder
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			text.WriteRune(b.Cell(x, y).Rune)
		}
		text.WriteByte('\n')
	}
	if !strings.Contains(text.String(), "Goroutines") || !strings.Contains(text.String(), "12 total") {
		t.Fatalf("the overlay is not drawn:\n%s", text.String())
	}
	pressRune(m, 'g')
	if m.panel != "" || m.rt.showing {
		t.Fatalf("a second g did not close it: panel %q showing %v", m.panel, m.rt.showing)
	}
}

// Esc and a click close the overlay and stop the profile fetch.
func TestGoroutineOverlayClosesWithEscAndClick(t *testing.T) {
	for name, closeIt := range map[string]func(*topModel){
		"esc": func(m *topModel) { m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc}) },
		"click": func(m *topModel) {
			m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: 5, Y: 5})
		},
	} {
		m, _ := liveModel(t, 1e9)
		m.rt.ok, m.rt.gOK, m.withInternals = true, true, false
		pressRune(m, 'g')
		closeIt(m)
		if m.panel != "" || m.rt.showing {
			t.Errorf("%s: panel %q showing %v", name, m.panel, m.rt.showing)
		}
	}
}

// When the Internals panel is on screen, g still switches its rows.
func TestGoroutineKeySwitchesThePanelRowsWhenItIsShown(t *testing.T) {
	m, _ := liveModel(t, 1e9)
	m.rt.ok, m.rt.gOK, m.withInternals = true, true, true
	pressRune(m, 'g')
	if m.panel != "" || !m.rt.showing {
		t.Fatalf("panel %q showing %v", m.panel, m.rt.showing)
	}
}

// The menu says in words what the selected item changes, with its value, so the
// fade time explains itself.
func TestMenuHintExplainsTheFadeTime(t *testing.T) {
	m, _ := liveModel(t, 1e9)
	m.proxyAfterglowSeconds = 10
	m.menu, m.menuSel = true, int(topMenuAfterglow)
	b := tui.New(100, 30)
	m.render(b)
	var text strings.Builder
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			text.WriteRune(b.Cell(x, y).Rune)
		}
		text.WriteByte('\n')
	}
	for _, want := range []string{"fades out over 10 s", "Steady traffic stays lit"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the menu does not say %q:\n%s", want, text.String())
		}
	}
}
