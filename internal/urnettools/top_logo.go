package urnettools

import (
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/brand"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The URnetwork logo inside `urnet-tools top`: a splash when top starts and a
// dim watermark in a graph that has no data yet, and the About window on A.
// The
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

// drawWatermark draws the 12x6 icon dim and centered in the rows of b from
// top down, behind the empty state of a graph that has no data yet. A plot
// too small for the icon gets nothing. The caller draws it only on the empty
// path, so the first data drawn takes it away.
func (m *topModel) drawWatermark(b *tui.Buffer, top int) {
	if !m.logoDrawable() {
		return
	}
	a := brand.Assets["icon-flat-12x6"]
	rows := b.Height() - top
	if a == nil || b.Width() < a.Cols || rows < a.Rows {
		return
	}
	// The glyphs only: the theme's dim style carries the color, so the icon
	// never stands out from the empty plot.
	grid := brand.Grid(a, brand.Capabilities{Depth: brand.Mono, NoColor: true, UTF8: true})
	dim := m.theme.Dim
	drawLogoGrid(b, (b.Width()-a.Cols)/2, top+(rows-a.Rows)/2, grid, &dim)
}

// topRepoName is the repository the About window names.
const topRepoName = "full-bars/urnetwork-3.23-fix"

// topLogoOffHint is the About window's line on how to turn the logo off.
const topLogoOffHint = "Logo off: --no-logo, URNET_NO_LOGO=1 or ui.logo = off in top.conf"

// topBuildInfo returns the commit the binary was built from, when the build
// recorded it, with "+changes" when the tree had uncommitted changes.
var topBuildInfo = func() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return "", false
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return "", false
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "+changes"
	}
	return rev, true
}

// topBuildLine is the build line of the About window: the commit when the
// build recorded one, the Go version and the platform.
func topBuildLine() string {
	line := "built"
	if rev, ok := topBuildInfo(); ok {
		line += " from " + rev
	}
	return line + " with " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
}

// aboutHandle applies an event while the About window is open. Esc or any
// other key closes it, and so does a click outside it. The closing event does
// nothing else. Ctrl-C still quits.
func (m *topModel) aboutHandle(ev tcellui.Event) (topEffect, bool) {
	if !m.about {
		return topNone, false
	}
	switch ev.Kind {
	case tcellui.EventKey:
		if ev.Key == tcellui.KeyCtrlC {
			return topNone, false
		}
		m.about = false
	case tcellui.EventRune:
		m.about = false
	case tcellui.EventMouse:
		r := m.aboutRect
		inside := ev.X >= r.X && ev.X < r.X+r.W && ev.Y >= r.Y && ev.Y < r.Y+r.H
		if ev.Action == tcellui.MousePress && !inside {
			m.about = false
		}
	default:
		return topNone, false
	}
	return topNone, true
}

// aboutArt picks the largest artwork that fits a window of the given size
// next to the text rows: the wide wordmark on a wide screen, the icon on a
// standard one, and nothing when the logo is off or cannot be shown.
func (m *topModel) aboutArt(w, h, textRows int) *brand.Asset {
	if !m.logoDrawable() {
		return nil
	}
	for _, name := range []string{"wordmark-100x7", "wordmark-80x6", "icon-flat-12x6", "icon-flat-8x4"} {
		a := brand.Assets[name]
		if a != nil && a.Cols+4 <= w && a.Rows+textRows+3 <= h {
			return a
		}
	}
	return nil
}

// drawAbout draws the About window centered over whatever is on screen: the
// logo, the version and build lines, the repository and how to turn the logo
// off.
func (m *topModel) drawAbout(b *tui.Buffer) {
	th := m.theme
	text := []string{"urnet-tools " + ToolVersion, topBuildLine(), topRepoName, "", topLogoOffHint, "", "Esc or any key closes this window"}
	a := m.aboutArt(b.Width(), b.Height(), len(text)+1)
	if a == nil || strings.HasPrefix(a.Name, "icon") {
		// The icon carries no name and no art carries nothing, so the
		// window names the network in words.
		text = append([]string{"URnetwork"}, text...)
	}
	w := 0
	for _, l := range text {
		w = max(w, tui.StringWidth(l))
	}
	artRows := 0
	if a != nil {
		w = max(w, a.Cols)
		artRows = a.Rows + 1
	}
	w = min(w+4, b.Width())
	h := min(len(text)+artRows+2, b.Height())
	r := tui.Rect{X: (b.Width() - w) / 2, Y: (b.Height() - h) / 2, W: w, H: h}
	m.aboutRect = r
	panel := b.Sub(r)
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "About", th.Frame, th.Border, th.Accent, th.ASCII)
	y := 0
	if a != nil {
		drawLogoGrid(in, (in.Width()-a.Cols)/2, 0, brand.Grid(a, m.logoCaps), nil)
		y = artRows
	}
	for i, l := range text {
		st := tui.Style{}
		switch {
		case l == "URnetwork" || strings.HasPrefix(l, "urnet-tools "):
			st = th.Accent
		case i == len(text)-1:
			st = th.Dim
		}
		l = tui.Truncate(l, in.Width(), th.ASCII)
		in.Put(l, max(0, (in.Width()-tui.StringWidth(l))/2), y+i, st)
	}
}
