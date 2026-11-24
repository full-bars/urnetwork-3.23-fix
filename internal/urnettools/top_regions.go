package urnettools

import "github.com/urnetwork/connect/internal/tui"

// topRegion is one named hit-test area recorded for a frame. The renderer
// fills m.regions while it draws; hitRegion maps a cell to the region under it.
type topRegion struct {
	Name string
	Rect tui.Rect
}

// hitRegion returns the name of the region containing (x, y), or "".
// Regions recorded later win over earlier ones, so titles beat boxes and
// dividers beat both.
func hitRegion(regions []topRegion, x, y int) string {
	for i := len(regions) - 1; i >= 0; i-- {
		r := regions[i]
		if x >= r.Rect.X && x < r.Rect.X+r.Rect.W && y >= r.Rect.Y && y < r.Rect.Y+r.Rect.H {
			return r.Name
		}
	}
	return ""
}

// record is a helper for the renderer: records a named region with its rect.
func (m *topModel) record(name string, rect tui.Rect) {
	m.regions = append(m.regions, topRegion{Name: name, Rect: rect})
}

// topFooterHint is one clickable footer hint.
type topFooterHint struct {
	name  string
	label string
}

// footerHints returns the clickable hints at the bottom of the screen, in
// draw order. The mode hint comes early so a narrow terminal drops the zoom
// and interval hints before it.
func (m *topModel) footerHints() []topFooterHint {
	hints := []topFooterHint{{"footer:quit", "q quit"}}
	if len(m.providers) > 1 {
		hints = append(hints, topFooterHint{"footer:provider", "tab provider"})
	}
	// The page hints are on every page with more than one to offer, whether the
	// strip row is drawn or hidden: the hints are the way back to both the
	// pages and the tabs.
	if pages, _ := m.pageStrip(m.pageArea()); len(pages) > 1 {
		hints = append(hints, topFooterHint{"footer:pages", "[/] page"}, topFooterHint{"footer:tabs", "p tabs"})
	}
	hints = append(hints, topFooterHint{"footer:help", "? help"}, topFooterHint{"footer:menu", "m menu"})
	// The banner's own hint is the main way to the fix; this footer hint names
	// the key while it applies and drops off with the banner.
	if m.lingerBannerActive() && m.lingerFixable {
		hints = append(hints, topFooterHint{"footer:linger", "L fix"})
	}
	// The mode hint names the mode K would switch to; the marker at the right
	// end of the row names the mode that is on.
	if m.mouse {
		hints = append(hints, topFooterHint{"footer:mode", topModeHintMouse})
	} else {
		hints = append(hints, topFooterHint{"footer:mode", topModeHintKeys})
	}
	if m.trafficOK {
		hints = append(hints, topFooterHint{"footer:zoom", "w zoom " + m.graphWindowLabel()})
	}
	if m.rt.ok && m.rt.gOK && m.hasInternals() {
		hints = append(hints, topFooterHint{"footer:goroutines", "g goroutines"})
	}
	hints = append(hints, topFooterHint{"footer:interval", "- " + topIntervalText(m.interval) + " +"})
	return hints
}

// modeMarker names the mode the terminal is in, for the right end of the
// footer row.
func (m *topModel) modeMarker() string {
	if m.mouse {
		return topModeLabelMouse
	}
	return topModeLabelKeys
}

// footerHintPlace is one hint and where it lands on the footer row.
type footerHintPlace struct {
	hint topFooterHint
	rect tui.Rect
}

// footerHintPlaces lays the hints out from column 1 with three spaces between
// them and stops at the first one that does not fit whole, so a narrow
// terminal loses whole hints instead of cutting one in half. The drawn row and
// the clickable regions come from this one list, so a click lands where the
// operator sees the label.
func footerHintPlaces(f tui.Rect, hints []topFooterHint) []footerHintPlace {
	out := make([]footerHintPlace, 0, len(hints))
	x := 1
	for i, h := range hints {
		if i > 0 {
			x += 3 // the gap between hints
		}
		w := tui.StringWidth(h.label)
		if x+w > f.W {
			break
		}
		out = append(out, footerHintPlace{hint: h, rect: tui.Rect{X: f.X + x, Y: f.Y, W: w, H: 1}})
		x += w
	}
	return out
}
