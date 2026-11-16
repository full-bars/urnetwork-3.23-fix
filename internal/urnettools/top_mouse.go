package urnettools

import (
	"time"

	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// topDoubleClick is how close two presses on the same divider must be to
// count as a double click, which resets the divider to its default.
const topDoubleClick = 500 * time.Millisecond

// topDrag tracks one in-progress divider drag.
type topDrag struct {
	region string
	at     time.Time
}

// topPress is the last divider press, for double-click detection.
type topPress struct {
	region string
	at     time.Time
}

// mouseHandle applies one mouse event. Stage 4 covers the dividers; the
// clickable hints, tabs and titles ride on the same hit map.
func (m *topModel) mouseHandle(ev tcellui.Event) topEffect {
	name := hitRegion(m.regions, ev.X, ev.Y)
	switch ev.Action {
	case tcellui.MouseMove:
		return topNone
	case tcellui.MousePress:
		if ev.Button != tcellui.MouseLeft {
			return topNone
		}
		now := m.now()
		// A second press on the same divider inside the double-click window
		// puts it back to its default.
		if isDivider(name) && m.lastPress.region == name && now.Sub(m.lastPress.at) < topDoubleClick {
			m.resetDivider(name)
			m.lastPress = topPress{}
			m.drag = nil
			m.saveLayout()
			return topNone
		}
		m.lastPress = topPress{region: name, at: now}
		if isDivider(name) {
			m.drag = &topDrag{region: name, at: now}
		}
		return topNone
	case tcellui.MouseDrag:
		if m.drag == nil {
			return topNone
		}
		m.moveDivider(m.drag.region, ev.X, ev.Y)
		return topNone
	case tcellui.MouseRelease:
		if m.drag != nil {
			m.drag = nil
			m.saveLayout()
		}
		return topNone
	}
	return topNone
}

// resetDivider puts one divider back to its default: the side column follows
// the terminal width again, and a box band gives the graph its flex share.
func (m *topModel) resetDivider(name string) {
	if name == "side" {
		m.layout.SideWidth = 0
		return
	}
	if m.layout.Dividers != nil {
		delete(m.layout.Dividers, name)
		if len(m.layout.Dividers) == 0 {
			m.layout.Dividers = nil
		}
	}
}

// saveLayout persists the arrangement with the same settings file the menu
// theme uses. A failed save must never stop top; the choice is a display
// preference.
func (m *topModel) saveLayout() {
	if m.settingsPath == "" {
		return
	}
	_ = saveTopSettings(m.settingsPath, m.settings())
}
