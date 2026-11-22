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
}
