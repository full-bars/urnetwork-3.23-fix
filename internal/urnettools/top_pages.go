package urnettools

import (
	"fmt"

	"github.com/urnetwork/connect/internal/tui"
)

// The page table behind `urnet-tools top`. A page owns a draw function that
// paints inside the content rectangle the frame hands it, and a predicate that
// says whether it has data for the current snapshot and size. The header, the
// page strip, the footer, the alert banner and the popups belong to the frame
// and are shared by every page, so adding a page is one file with its draw
// function and one line in topPages.

// topPageStripRows is the height of the page strip band.
const topPageStripRows = 1

// topStripMinHeight is the shortest terminal that gives the strip its row. The
// strip is part of the full layout, which needs topMinHeight rows; below that
// the compact view keeps every row for its graph and offers no tabs.
const topStripMinHeight = topMinHeight

// The one-line messages p shows when it hides or shows the strip.
const (
	topNoticeTabsHidden = "Page tabs hidden. p shows them"
	topNoticeTabsShown  = "Page tabs shown. p hides them"
)

// topPage is one page of the dashboard.
type topPage struct {
	// id names the page in hit regions and in the page key path.
	id string
	// title is the short name the strip shows.
	title string
	// available reports whether the page has something to show for this model
	// and a content rectangle of this size. A page without data is left out of
	// the strip, unless it is the page on screen.
	available func(m *topModel, content tui.Rect) bool
	// draw paints the page. b is the whole screen buffer and content is the
	// rectangle the page may use, in absolute cells: a page draws through
	// b.Sub(content) so it cannot spill into the chrome, and records its hit
	// regions in absolute cells.
	draw func(m *topModel, b *tui.Buffer, content tui.Rect)
}

// topPages is the page table, in strip order. The first entry is the dashboard
// every earlier version of top drew, and it stays the default page.
var topPages = []topPage{
	{id: "overview", title: "Overview", available: func(*topModel, tui.Rect) bool { return true }, draw: (*topModel).drawDashboard},
	{id: "remote", title: "Remote", available: (*topModel).remotePageAvailable, draw: (*topModel).drawRemotePage},
	{id: "billable", title: "Billable", available: (*topModel).billablePageAvailable, draw: (*topModel).drawBillablePage},
	{id: "total", title: "Total", available: (*topModel).totalPageAvailable, draw: (*topModel).drawTotalPage},
}

// pageIndex is the selected page's table index, clamped to the table.
func (m *topModel) pageIndex() int {
	if m.page < 0 || m.page >= len(topPages) {
		return 0
	}
	return m.page
}

// selectedPage returns the page on screen.
func (m *topModel) selectedPage() topPage { return topPages[m.pageIndex()] }

// pageArea is the content rectangle the last frame drew into, or a standard
// size before the first frame, so an availability query always has an answer.
func (m *topModel) pageArea() tui.Rect {
	if m.contentRect.Empty() {
		return tui.Rect{W: 80, H: 24}
	}
	return m.contentRect
}

// pageStrip lists the table indices of the pages the strip offers for a
// content rectangle of this size, in table order, plus the position of the
// selected page in that list. The selected page is always present, so a page
// whose data has gone keeps the screen until the operator moves off it.
func (m *topModel) pageStrip(content tui.Rect) ([]int, int) {
	out := make([]int, 0, len(topPages))
	current := 0
	sel := m.pageIndex()
	for i, p := range topPages {
		if i != sel && p.available != nil && !p.available(m, content) {
			continue
		}
		if i == sel {
			current = len(out)
		}
		out = append(out, i)
	}
	return out, current
}

// pageKeysActive reports whether a page switch key should act. The keys belong
// to the overview, the help and menu overlays and the details popups while they
// are open, and the compact view below the full layout's minimum has no strip
// to switch, so it stays on the dashboard.
func (m *topModel) pageKeysActive() bool {
	if m.compact || m.overview || m.help || m.menu || m.panel != "" || m.proxyPopup {
		return false
	}
	return true
}

// stripShown reports whether a frame draws the page strip row: the strip is on
// by default, the operator has not hidden it with p, there is more than one
// page to offer, and the terminal is tall enough to give the row to the strip
// instead of the compact view's graph.
func (m *topModel) stripShown(r tui.Rect, pages int) bool {
	return pages > 1 && !m.stripHidden && r.H >= topStripMinHeight
}

// togglePageStrip hides or shows the page strip for the session and says which
// way it went. The choice is not saved: a new top starts with the strip on,
// and with a single page there is nothing to hide.
func (m *topModel) togglePageStrip() {
	if pages, _ := m.pageStrip(m.pageArea()); len(pages) < 2 {
		return
	}
	m.stripHidden = !m.stripHidden
	if m.stripHidden {
		m.setNotice(topNoticeTabsHidden)
		return
	}
	m.setNotice(topNoticeTabsShown)
}

// stepPage moves the selection by delta among the pages the strip offers,
// wrapping at either end. One page is nowhere to go.
func (m *topModel) stepPage(delta int) {
	idx, current := m.pageStrip(m.pageArea())
	if len(idx) < 2 {
		return
	}
	n := len(idx)
	m.page = idx[((current+delta)%n+n)%n]
}

// selectPage jumps to the page at a table index, when the strip offers it.
func (m *topModel) selectPage(i int) {
	idx, _ := m.pageStrip(m.pageArea())
	for _, k := range idx {
		if k == i {
			m.page = i
			return
		}
	}
}

// selectPageID jumps to the page with this id, when the strip offers it.
func (m *topModel) selectPageID(id string) {
	for i, p := range topPages {
		if p.id == id {
			m.selectPage(i)
			return
		}
	}
}

// topPageLabel is the strip label of the page at table index i. The number is
// the table position, so it does not move when another page loses its data.
func topPageLabel(i int, p topPage) string {
	return fmt.Sprintf("[%d %s]", i+1, p.title)
}

// topStripFit picks which tabs to draw in width cells, always keeping the
// selected one. It widens out from the selection one page each side per step,
// so a narrow strip shows the pages next to the one on screen instead of the
// first pages of the table. The selected label is kept even when it alone is
// too wide; the draw truncates it.
func topStripFit(labels []string, current, width int) []int {
	if current < 0 || current >= len(labels) {
		return nil
	}
	keep := make([]bool, len(labels))
	keep[current] = true
	used := tui.StringWidth(labels[current])
	add := func(i int) bool {
		w := tui.StringWidth(labels[i]) + 1 // the space before the tab
		if used+w > width {
			return false
		}
		used += w
		keep[i] = true
		return true
	}
	for d := 1; ; d++ {
		left, right := current-d, current+d
		if left < 0 && right >= len(labels) {
			break
		}
		added := false
		if right < len(labels) && add(right) {
			added = true
		}
		if left >= 0 && add(left) {
			added = true
		}
		if !added {
			break
		}
	}
	out := make([]int, 0, len(labels))
	for i, ok := range keep {
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// drawPageStrip paints the strip band and records one hit region per tab. The
// selected tab is drawn in reverse video on a colour theme and with a leading
// star on a theme that has neither colour nor attributes.
func (m *topModel) drawPageStrip(band tui.Rect, b *tui.Buffer, idx []int, current int) {
	th := m.theme
	s := b.Sub(band)
	labels := make([]string, len(idx))
	for k, i := range idx {
		labels[k] = topPageLabel(i, topPages[i])
	}
	x := 1
	for _, k := range topStripFit(labels, current, max(s.Width()-1, 0)) {
		label := labels[k]
		style := th.Dim
		if k == current {
			if th.ASCII {
				label = "*" + label
			} else {
				style = th.Accent.With(tui.AttrReverse)
			}
		} else if m.hover == "page:"+topPages[idx[k]].id {
			style = th.Accent.With(tui.AttrUnderline)
		}
		w := min(tui.StringWidth(label), max(s.Width()-x, 0))
		if w > 0 {
			m.record("page:"+topPages[idx[k]].id, tui.Rect{X: band.X + x, Y: band.Y, W: w, H: topPageStripRows})
		}
		x = s.Put(tui.Truncate(label, max(s.Width()-x, 0), th.ASCII), x, 0, style) + 1
	}
}

// pageByID is the table index of the page with this id, or -1.
func pageByID(id string) int {
	for i, p := range topPages {
		if p.id == id {
			return i
		}
	}
	return -1
}
