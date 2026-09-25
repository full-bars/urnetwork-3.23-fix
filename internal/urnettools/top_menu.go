package urnettools

import (
	"slices"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The `m` menu: pick the color theme and the graph style. Changes apply at once,
// so the screen behind the menu is the preview, and are saved as they are made.

const (
	topMenuTheme = iota
	topMenuGraph
	topMenuRows
)

// applySettings adopts saved settings. The theme is left alone when the
// environment forced one (NO_COLOR, a dumb terminal): that is the user's
// explicit request and outranks a saved preference.
func (m *topModel) applySettings(s topSettings) {
	if s.Theme != "" && !m.themeLocked {
		if th, ok := tui.ThemeByName(s.Theme); ok {
			m.theme = th
		}
	}
	if g, ok := tui.GraphSymbolsByName(s.Graph); ok {
		m.graph = g
	}
}

func (m *topModel) settings() topSettings {
	return topSettings{Theme: m.theme.Name, Graph: m.graph.String()}
}

// menuHandle applies one input event while the menu is open. It reports whether
// the menu consumed it; quit keys pass through so q and Ctrl-C always work.
func (m *topModel) menuHandle(ev tcellui.Event) (consumed bool) {
	switch ev.Kind {
	case tcellui.EventKey:
		switch ev.Key {
		case tcellui.KeyCtrlC:
			return false
		case tcellui.KeyEsc:
			m.menu = false
		case tcellui.KeyUp, tcellui.KeyBacktab:
			m.menuMove(-1)
		case tcellui.KeyDown, tcellui.KeyTab:
			m.menuMove(1)
		case tcellui.KeyLeft:
			m.menuCycle(-1)
		case tcellui.KeyRight, tcellui.KeyEnter:
			m.menuCycle(1)
		}
		return true
	case tcellui.EventRune:
		switch ev.Rune {
		case 'q', 'Q':
			return false
		case 'm', 'M':
			m.menu = false
		case 'k', 'K':
			m.menuMove(-1)
		case 'j', 'J':
			m.menuMove(1)
		case 'h', 'H':
			m.menuCycle(-1)
		case 'l', 'L', ' ':
			m.menuCycle(1)
		}
		return true
	}
	return false
}

func (m *topModel) menuMove(d int) {
	m.menuSel = ((m.menuSel+d)%topMenuRows + topMenuRows) % topMenuRows
	m.menuNote = ""
}

// menuCycle steps the selected row one choice along its list, wrapping.
func (m *topModel) menuCycle(d int) {
	m.menuNote = ""
	switch m.menuSel {
	case topMenuTheme:
		if m.themeLocked {
			m.menuNote = "theme fixed by NO_COLOR or TERM"
			return
		}
		names := tui.ThemeNames()
		i := slices.Index(names, m.theme.Name)
		th, _ := tui.ThemeByName(names[((i+d)%len(names)+len(names))%len(names)])
		m.theme = th
	case topMenuGraph:
		n := len(tui.GraphSymbolNames)
		m.graph = tui.GraphSymbols(((int(m.graph)+d)%n + n) % n)
	}
	if err := saveTopSettings(m.settingsPath, m.settings()); err != nil {
		m.menuNote = "could not save: " + err.Error()
	}
}

// drawMenu draws the menu centered over the screen.
func (m *topModel) drawMenu(b *tui.Buffer) {
	th := m.theme
	type row struct{ label, value string }
	rows := [topMenuRows]row{
		topMenuTheme: {"Theme", m.theme.Name},
		topMenuGraph: {"Graph style", m.graph.String()},
	}
	const w = 46
	h := topMenuRows + 6
	note := m.menuNote
	switch {
	case note != "":
	case m.themeLocked:
		note = "theme fixed by NO_COLOR or TERM"
	case th.ASCII && m.graph != tui.GraphTTY:
		note = "this terminal draws graphs as tty"
	}
	rect := tui.Rect{X: max((b.Width()-w)/2, 0), Y: max((b.Height()-h)/2, 0), W: min(w, b.Width()), H: min(h, b.Height())}
	panel := b.Sub(rect)
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "Menu", th.Frame, th.Border, th.Accent, th.ASCII)

	marker, left, right := "▸ ", "◂ ", " ▸"
	if th.ASCII {
		marker, left, right = "> ", "< ", " >"
	}
	for i, r := range rows {
		style, lead := th.Dim, "  "
		if i == m.menuSel {
			style, lead = th.Accent, marker
		}
		x := in.Put(lead+r.label, 0, i, style)
		vx := max(x, 20)
		in.Put(tui.Truncate(left+r.value+right, max(in.Width()-vx, 0), th.ASCII), vx, i, tui.Style{})
	}
	// The palette as the screen will use it: one word per role, in its own style.
	y := topMenuRows + 1
	x := in.Put("  ", 0, y, tui.Style{})
	for _, sw := range []struct {
		word  string
		style tui.Style
	}{{"ok", th.OK}, {"warn", th.Warn}, {"bad", th.Bad}, {"accent", th.Accent}, {"graph", th.Graph}, {"dim", th.Dim}} {
		x = in.Put(sw.word+" ", x, y, sw.style)
	}
	if note != "" {
		in.Put(tui.Truncate(" "+note, in.Width(), th.ASCII), 0, y+1, th.Warn)
	}
	keys := " ↑↓ select   ←→ change   Esc close"
	if th.ASCII {
		keys = " j/k select   h/l change   Esc close"
	}
	in.Put(tui.Truncate(keys, in.Width(), th.ASCII), 0, in.Height()-1, th.Dim)
}
