package urnettools

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
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

// mouseHandle applies one mouse event. It returns the effect to run (quit),
// like the key handlers.
func (m *topModel) mouseHandle(ev tcellui.Event) topEffect {
	// While the help overlay is up, a click closes it and nothing else
	// happens, like Esc.
	if m.help || m.panel != "" {
		switch {
		case ev.Action == tcellui.MousePress:
			m.help = false
			if m.panel == "goroutines" {
				m.rt.showing = false
			}
			m.panel = ""
		case ev.Action == tcellui.MouseWheel && m.panel == "goroutines" && m.rt.groups != nil:
			dir := 1
			if ev.Button == tcellui.MouseWheelUp {
				dir = -1
			}
			m.gorScroll = min(max(m.gorScroll+dir, 0), max(0, len(m.rt.groups.Groups)-m.gorHeight))
		}
		return topNone
	}
	name := hitRegion(m.regions, ev.X, ev.Y)
	switch ev.Action {
	case tcellui.MouseMove:
		m.proxyPopupAt(ev.X, ev.Y)
		m.hover = name
		return topNone
	case tcellui.MousePress:
		if ev.Button != tcellui.MouseLeft {
			return topNone
		}
		if m.proxyPopupAt(ev.X, ev.Y) {
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
			return topNone
		}
		return m.mouseClick(name, ev)
	case tcellui.MouseDrag:
		if m.drag == nil {
			return topNone
		}
		before := m.layout.Dividers[m.drag.region]
		beforeWidth := m.layout.SideWidth
		m.moveDivider(m.drag.region, ev.X, ev.Y)
		if m.layout.Dividers[m.drag.region] != before || m.layout.SideWidth != beforeWidth {
			m.lastPress = topPress{}
		}
		return topNone
	case tcellui.MouseRelease:
		if m.drag != nil {
			m.drag = nil
			m.saveLayout()
		}
		return topNone
	case tcellui.MouseWheel:
		m.mouseWheel(name, ev)
		return topNone
	}
	return topNone
}

// mouseClick dispatches a click on a clickable region.
func (m *topModel) mouseClick(name string, ev tcellui.Event) topEffect {
	if id, ok := strings.CutPrefix(name, "page:"); ok {
		m.selectPageID(id)
		return topNone
	}
	switch name {
	case "footer:pages":
		m.stepPage(1)
	case "footer:quit":
		m.quit = true
		return topQuit
	case "footer:provider", "tab:provider":
		return m.selectProvider(1)
	case "footer:help":
		m.help = !m.help
	case "footer:menu":
		m.menu, m.help, m.menuNote = true, false, ""
	case "footer:zoom", "title:graph":
		m.zoomOn = !m.zoomOn
	case "footer:goroutines":
		// Only where the provider can answer, like the g key.
		if m.rt.gOK && m.rt.ok {
			m.rt.showing = !m.rt.showing
			m.rt.gLast = time.Time{}
			m.gorScroll = 0
		}
	case "footer:interval":
		for _, r := range m.regions {
			if r.Name == name {
				if ev.X < r.Rect.X+r.Rect.W/2 {
					m.stepInterval(-1) // the - half polls faster
				} else {
					m.stepInterval(1) // the + half polls slower
				}
				break
			}
		}
	case "footer:mode":
		m.toggleMouseMode()
	}
	return topNone
}

// mouseWheel scrolls the list the pointer is over.
func (m *topModel) mouseWheel(name string, ev tcellui.Event) {
	dir := 1
	if ev.Button == tcellui.MouseWheelUp {
		dir = -1
	}
	switch name {
	case "box:events":
		m.scrollEvents(dir)
	case "box:internals":
		if m.rt.showing && m.rt.groups != nil {
			m.gorScroll = min(max(m.gorScroll+dir, 0), max(0, len(m.rt.groups.Groups)-m.gorHeight))
		}
	}
}

// clampScroll keeps a wheel offset inside the list.
func clampScroll(v, total int) int {
	if v < 0 {
		return 0
	}
	return min(v, max(0, total-1))
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

// hoverStyle returns the accent style for the region under the cursor, or the
// base style otherwise. The mono theme has no color and no attributes, so the
// hover leaves the base style untouched there.
func (m *topModel) hoverStyle(region string, base tui.Style) tui.Style {
	if m.hover == region && !m.theme.ASCII {
		return m.theme.Accent.With(tui.AttrUnderline)
	}
	return base
}

// drawHover paints the divider under the cursor so an operator can see it is
// draggable. The mono theme leaves the divider alone.
func (m *topModel) drawHover(b *tui.Buffer) {
	if m.hover == "" || m.theme.ASCII || !isDivider(m.hover) {
		return
	}
	for _, r := range m.regions {
		if r.Name != m.hover {
			continue
		}
		for y := r.Rect.Y; y < r.Rect.Y+r.Rect.H; y++ {
			for x := r.Rect.X; x < r.Rect.X+r.Rect.W; x++ {
				c := b.Cell(x, y)
				if c.Continuation() {
					continue
				}
				b.Set(x, y, tui.Cell{Rune: c.Rune, Comb: c.Comb, Style: m.theme.Accent})
			}
		}
		return
	}
}

// hoverSignature names everything a mouse move can change on screen: the hover
// highlight and the proxy popup. The run loop redraws for a move only when it
// changed, so a pointer drifting inside one region costs nothing.
func (m *topModel) hoverSignature() string {
	return fmt.Sprint(m.hover, "|", m.proxyPopup, m.proxyPopupX, m.proxyPopupY, m.proxyPopupIndices)
}
