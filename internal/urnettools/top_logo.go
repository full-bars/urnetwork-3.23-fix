package urnettools

import (
	"time"

	"github.com/urnetwork/connect/internal/brand"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The URnetwork logo inside `urnet-tools top`: a splash when top starts. The
// artwork comes from internal/brand and the switches from logo.go, so top
// follows the same rules as the rest of the tool.

// topSplashDuration is how long the splash stays up when no key skips it.
const topSplashDuration = 1500 * time.Millisecond

// topSplash is the logo shown once when top starts. armed is set at start
// when the logo may show, until is set by the first frame, and done is final:
// nothing sets armed again, so a provider switch or a reconnect never brings
// the splash back.
type topSplash struct {
	armed bool
	until time.Time
	done  bool
}

// enableLogo decides once, at start, whether this run may draw the logo. The
// switches are the ones every other command honors, plus --copy, whose saved
// screen is read as text.
func (m *topModel) enableLogo(plain, copyOnQuit bool) {
	m.logo = logoAllowed(logoMode{Plain: plain}) && !copyOnQuit
	m.logoCaps = logoCapabilities()
	m.splash.armed = m.logo
}

// logoDrawable reports whether this frame may draw block art: the run allows
// the logo and the theme is not the ASCII one, whose terminal cannot show it.
func (m *topModel) logoDrawable() bool {
	return m.logo && m.logoCaps.UTF8 && !m.theme.ASCII
}

// splashActive reports whether this frame is the splash. The first call
// starts its clock, so the time top spends before the first frame does not
// count. A terminal too small for every asset ends it at once.
func (m *topModel) splashActive(r tui.Rect) bool {
	s := &m.splash
	if !s.armed || s.done {
		return false
	}
	if !m.logoDrawable() || brand.Choose(r.W, r.H) == nil {
		s.done = true
		return false
	}
	now := m.now()
	if s.until.IsZero() {
		s.until = now.Add(topSplashDuration)
	}
	if !now.Before(s.until) {
		s.done = true
		return false
	}
	return true
}

// splashHandle lets any key or click skip the splash. The event only skips:
// it does not also act on the dashboard behind it. Ctrl-C still quits.
func (m *topModel) splashHandle(ev tcellui.Event) (topEffect, bool) {
	if !m.splash.armed || m.splash.done {
		return topNone, false
	}
	switch ev.Kind {
	case tcellui.EventKey:
		if ev.Key == tcellui.KeyCtrlC {
			return topNone, false
		}
	case tcellui.EventRune:
	case tcellui.EventMouse:
		if ev.Action != tcellui.MousePress {
			return topNone, true
		}
	default:
		return topNone, false
	}
	m.splash.done = true
	return topNone, true
}

// drawSplash draws the asset the brand package picks for the screen size,
// centered.
func (m *topModel) drawSplash(b *tui.Buffer) {
	a := brand.Choose(b.Width(), b.Height())
	if a == nil {
		return
	}
	drawLogoGrid(b, (b.Width()-a.Cols)/2, (b.Height()-a.Rows)/2, brand.Grid(a, m.logoCaps), nil)
}

// drawLogoGrid paints artwork cells at x, y. With style set, every glyph is
// drawn in that one style; otherwise each cell keeps the artwork's colors.
// Blank cells are skipped, so what is under them shows through.
func drawLogoGrid(b *tui.Buffer, x, y int, grid [][]brand.Cell, style *tui.Style) {
	for ry, row := range grid {
		for rx, c := range row {
			if c.Glyph == ' ' {
				continue
			}
			st := tui.Style{}
			if style != nil {
				st = *style
			} else {
				if c.FG != nil {
					st.FG = logoColor(c.FG)
				}
				if c.BG != nil {
					st.BG = logoColor(c.BG)
				}
			}
			b.Set(x+rx, y+ry, tui.Cell{Rune: c.Glyph, Style: st})
		}
	}
}

// logoColor turns an artwork color into a truecolor value. The terminal
// backend downgrades it to what the terminal supports.
func logoColor(rgb []int) tui.Color {
	return tui.RGB(uint8(rgb[0]), uint8(rgb[1]), uint8(rgb[2]))
}
