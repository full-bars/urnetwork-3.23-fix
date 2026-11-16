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
// draw order.
func (m *topModel) footerHints() []topFooterHint {
	hints := []topFooterHint{{"footer:quit", "q quit"}}
	if len(m.providers) > 1 {
		hints = append(hints, topFooterHint{"footer:provider", "tab provider"})
	}
	hints = append(hints, topFooterHint{"footer:help", "? help"}, topFooterHint{"footer:menu", "m menu"})
	if m.trafficOK {
		hints = append(hints, topFooterHint{"footer:zoom", "w zoom"})
	}
	if m.rt.ok && m.rt.gOK && m.hasInternals() {
		hints = append(hints, topFooterHint{"footer:goroutines", "g goroutines"})
	}
	hints = append(hints, topFooterHint{"footer:interval", "- " + topIntervalText(m.interval) + " +"})
	mouse := "mouse off"
	if m.mouse {
		mouse = "mouse on"
	}
	hints = append(hints, topFooterHint{"footer:mouse", "M " + mouse})
	return hints
}

// footerHintRects returns the rect of each footer hint in absolute terminal
// cells. The hints start at column 1 with three spaces between them.
func footerHintRects(f tui.Rect, hints []topFooterHint) []topRegion {
	out := make([]topRegion, len(hints))
	x := 1
	for i, h := range hints {
		w := tui.StringWidth(h.label)
		if x >= f.W {
			break
		}
		out[i] = topRegion{Name: h.name, Rect: tui.Rect{X: f.X + x, Y: f.Y, W: min(w, f.W-x), H: 1}}
		x += w + 3
	}
	return out
}
