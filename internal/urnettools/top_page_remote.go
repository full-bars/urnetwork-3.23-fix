package urnettools

import "github.com/urnetwork/connect/internal/tui"

// Page 2 of `urnet-tools top`: the mirrored transfer chart on a page of its
// own, for a window tall enough that the dashboard's shared graph band is a
// poor home for it. The chart itself is the one top_remote_panel.go draws, so
// the two places cannot drift. Key r still puts the same chart under the
// throughput graphs on page 1.

// remotePageAvailable reports whether the provider has transfer history to
// draw. The page keeps the chart's own empty state, but it is left out of the
// strip while the provider has never answered, so the strip never offers an
// empty page.
func (m *topModel) remotePageAvailable(tui.Rect) bool {
	_, _, _, _, ok := m.remoteData()
	return ok
}

// drawRemotePage paints page 2 inside the content rectangle.
func (m *topModel) drawRemotePage(b *tui.Buffer, content tui.Rect) {
	th := m.theme
	box := tui.DrawBox(b.Sub(content), "Remote", th.Frame, th.Border, th.Accent, th.ASCII)
	m.record("box:remote", content)
	m.drawRemote(box)
	m.drawRemoteTransports(box)
}

// drawRemoteTransports puts the Transports bar and its legend under the chart,
// the way the client app's Client statistics card stacks them: a title, the
// segmented bar, the kinds that carried traffic with their percent, then the
// unused kinds, dim. It uses the drawing of the Transports page. Nothing is
// drawn until the provider reports the transport block and a full window, or
// when the rows under the chart cannot hold the bar and one legend row.
func (m *topModel) drawRemoteTransports(box *tui.Buffer) {
	shares, live, _, ready, ok := m.transportsData()
	if !ok || !ready {
		return
	}
	l := remoteLayout(box.Width(), box.Height())
	if l.downPeak < 0 {
		return
	}
	y := l.downPeak + 2
	if y+3 > box.Height() {
		return
	}
	th := m.theme
	area := box.Sub(tui.Rect{Y: y, W: box.Width(), H: box.Height() - y})
	area.Put(tui.Truncate("Transports", area.Width(), th.ASCII), 0, 0, th.Accent)
	pal := transportsPalette(th)
	drawTransportBar(area, 1, shares, pal, th.ASCII)
	next := drawTransportLegend(area, 2, shares, live, pal, th)
	drawTransportUnused(area, next, shares, live, th)
}
