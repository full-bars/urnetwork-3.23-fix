package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// Every switching key works, wraps at both ends, and the digits jump straight
// to a page.
func TestTopPageKeys(t *testing.T) {
	cases := []struct {
		name  string
		event tcellui.Event
		from  int
		want  int
	}{
		{"] next", tcellui.Event{Kind: tcellui.EventRune, Rune: ']'}, 0, 1},
		{"[ back", tcellui.Event{Kind: tcellui.EventRune, Rune: '['}, 1, 0},
		{"] wraps", tcellui.Event{Kind: tcellui.EventRune, Rune: ']'}, 1, 0},
		{"[ wraps", tcellui.Event{Kind: tcellui.EventRune, Rune: '['}, 0, 1},
		{"2 jumps", tcellui.Event{Kind: tcellui.EventRune, Rune: '2'}, 0, 1},
		{"1 jumps", tcellui.Event{Kind: tcellui.EventRune, Rune: '1'}, 1, 0},
		{"pgdn", tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyPageDown}, 0, 1},
		{"pgup", tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyPageUp}, 1, 0},
	}
	for _, c := range cases {
		clock := &fakeClock{t: topBase}
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.apply(0, topFlowing(t), nil)
		m.page = c.from
		m.handle(c.event)
		if m.page != c.want {
			t.Errorf("%s: page %d, want %d", c.name, m.page, c.want)
		}
	}
}

// A page the provider cannot fill is not selectable, and a digit past the last
// page changes nothing.
func TestTopPageKeyRefusesAnUnavailablePage(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	pressRune(m, '2')
	if m.page != 0 {
		t.Fatalf("2 selected the unavailable Remote page")
	}
	m.apply(0, topFlowing(t), nil)
	pressRune(m, '9')
	if m.page != 0 {
		t.Fatalf("a digit past the last page selected %d", m.page)
	}
}

// While an overlay owns the screen the page keys belong to it.
func TestTopPageKeysIgnoredWhileAnOverlayIsOpen(t *testing.T) {
	for name, open := range map[string]func(*topModel){
		"help":     func(m *topModel) { m.help = true },
		"menu":     func(m *topModel) { m.menu = true },
		"panel":    func(m *topModel) { m.panel = "alerts" },
		"overview": func(m *topModel) { m.overview = true },
		"popup":    func(m *topModel) { m.proxyPopup = true },
		"compact":  func(m *topModel) { m.compact = true },
	} {
		for _, ev := range []func(*topModel){
			func(m *topModel) { pressRune(m, ']') },
			func(m *topModel) { pressSpecial(m, tcellui.KeyPageDown) },
			func(m *topModel) { pressRune(m, '2') },
		} {
			// A fresh model per event: the press that closes a popup is
			// swallowed, and the next press is a different decision.
			clock := &fakeClock{t: topBase}
			m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
			m.apply(0, topFlowing(t), nil)
			open(m)
			ev(m)
			if m.page != 0 {
				t.Errorf("%s open: a page key switched to page %d", name, m.page)
			}
		}
	}
}

// The strip tabs are hit regions, and a click on one switches page.
func TestTopPageStripClickSwitches(t *testing.T) {
	m, b, _ := pageModel(t, 120, 40)
	m.page = 1
	renderFresh(m, b)
	overview := regionRect(t, m, "page:overview")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, overview.X+2, overview.Y))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, overview.X+2, overview.Y))
	if m.page != 0 {
		t.Fatalf("the click left page %d, want 0", m.page)
	}
	renderFresh(m, b)
	if m.contentRect.Y != 2 {
		t.Fatalf("back on the dashboard the content starts at y=%d, want 2", m.contentRect.Y)
	}
}

// The page belongs to the session: switching provider redraws it, it does not
// fall back to the dashboard.
func TestTopPageSurvivesProviderSwitch(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.apply(0, topFlowing(t), nil)
	pressRune(m, '2')
	if m.page != 1 {
		t.Fatalf("page %d before the switch", m.page)
	}
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyTab})
	if m.cur != 1 {
		t.Fatalf("Tab left the provider at %d", m.cur)
	}
	if m.page != 1 {
		t.Fatalf("switching provider moved the page to %d, want 1", m.page)
	}
	// The new provider has not answered yet, so the Remote page shows its own
	// empty state and keeps the strip, rather than silently returning home.
	b := tui.New(120, 40)
	renderFresh(m, b)
	if !strings.Contains(b.String(), "[2 Remote]") {
		t.Fatalf("the strip lost the selected page:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "waiting for the provider") {
		t.Fatalf("the selected page did not draw its empty state:\n%s", b.String())
	}
}
