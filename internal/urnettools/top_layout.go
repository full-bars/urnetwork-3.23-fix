package urnettools

import (
	"strings"

	"github.com/urnetwork/connect/internal/tui"
)

// topLayout is the operator's arrangement of the top screen: how wide the
// right column is and where the box dividers sit. The zero value is the
// default, so a missing or corrupt settings file renders the default layout.
type topLayout struct {
	SideWidth int            // explicit right column width; 0 = follow the terminal width
	Dividers  map[string]int // divider name -> content row index
}

// topMinBoxRows is the smallest box the layout allows: its frame plus one
// row of content, so a dragged divider can never squeeze a box to a bare
// border.
const topMinBoxRows = 3

// topMinLeftCols is the smallest main column a dragged side divider leaves.
const topMinLeftCols = 40

// dividerRow returns the saved row for the named divider, or the default.
func (l topLayout) dividerRow(name string, def int) int {
	if v, ok := l.Dividers[name]; ok && v > 0 {
		return v
	}
	return def
}

// clampRow keeps a divider row between lo and hi. A column too short for the
// minimum collapses the band instead of overlapping it.
func clampRow(v, lo, hi int) int {
	if hi < lo {
		return 0
	}
	return min(max(v, lo), hi)
}

// clampSide keeps a dragged side width between the default minimum and the
// cap, and leaves the main column at least topMinLeftCols cells.
func clampSide(contentW, w int) int {
	maxW := min(topSideMaxWidth, max(contentW-topMinLeftCols, topSideWidth))
	return min(max(w, topSideWidth), maxW)
}

// sideCols resolves the side column width: the operator's explicit width when
// they dragged the divider, otherwise the width that follows the terminal.
func (m *topModel) sideCols(contentW int) int {
	if w := m.layout.SideWidth; w > 0 {
		return clampSide(contentW, w)
	}
	return topSideCols(contentW)
}

// leftDividerRows resolves the left column's two divider rows for a column of
// height h: where Proxies starts and where Events starts.
func (m *topModel) leftDividerRows(h int) (dProxies, dEvents int) {
	wantP, wantE := m.proxiesRows(), m.eventsRows()
	if wantP > topProxiesCompactRows && m.hasProxyGridData() {
		if avail := h - wantE - topGraphMinRows; avail >= proxyGridMinH {
			wantP = min(topProxiesGridRows, avail)
		}
	}
	dProxies = m.layout.dividerRow("left:proxies", h-wantP-wantE)
	dEvents = m.layout.dividerRow("left:events", h-wantE)
	dProxies = clampRow(dProxies, topMinBoxRows, h-2*topMinBoxRows)
	dEvents = clampRow(dEvents, dProxies+topMinBoxRows, h-topMinBoxRows)
	return dProxies, dEvents
}

// splitLeft cuts the main column into the graph, Proxies and Events bands.
// Without a saved divider it is exactly the old split: the graph takes what
// the two boxes leave.
func (m *topModel) splitLeft(r tui.Rect) []tui.Rect {
	h := r.H
	dProxies, dEvents := m.leftDividerRows(h)
	return tui.SplitRows(r, tui.Fixed(dProxies), tui.Fixed(dEvents-dProxies), tui.Fixed(h-dEvents))
}

// sideDividerRows resolves the side column's divider rows for the current
// draw: where Resources starts, where Extender starts and where Internals
// starts.
func (m *topModel) sideDividerRows(h int) (dRes, dExt, dInt int) {
	dRes = m.layout.dividerRow("side:resources", m.nowRows)
	dExt = m.layout.dividerRow("side:extender", m.nowRows+m.resRows)
	dInt = m.layout.dividerRow("side:internals", m.nowRows+m.resRows+m.extRows)
	dRes = clampRow(dRes, topMinBoxRows, m.resourcesDividerMax(h))
	if m.showExtender {
		remaining := topMinBoxRows
		if m.withInternals {
			remaining += topMinBoxRows
		}
		dExt = clampRow(dExt, dRes+topMinBoxRows, h-remaining)
		endMax := h
		if m.withInternals {
			endMax -= topMinBoxRows
		}
		dInt = clampRow(dInt, dExt+topMinBoxRows, endMax)
	}
	if m.withInternals {
		lo := dRes + topMinBoxRows
		if m.showExtender {
			lo = dExt + topMinBoxRows
		}
		dInt = clampRow(dInt, lo, h-topMinBoxRows)
	}
	return dRes, dExt, dInt
}

func (m *topModel) resourcesDividerMax(h int) int {
	boxes := 1
	if m.showExtender {
		boxes++
	}
	if m.withInternals {
		boxes++
	}
	return h - boxes*topMinBoxRows
}

// splitSide cuts the side column into its boxes. The default rows reproduce
// the old fixed split exactly, so a fresh settings file changes nothing.
func (m *topModel) splitSide(r tui.Rect) []tui.Rect {
	h := r.H
	dRes, dExt, dInt := m.sideDividerRows(h)
	switch {
	case m.showExtender && m.withInternals:
		return tui.SplitRows(r, tui.Fixed(dRes), tui.Fixed(dExt-dRes), tui.Fixed(dInt-dExt), tui.Flex(1))
	case m.showExtender:
		// The Extender box keeps its own size; what is left stays empty.
		return tui.SplitRows(r, tui.Fixed(dRes), tui.Fixed(dExt-dRes), tui.Fixed(dInt-dExt), tui.Flex(1))
	case m.withInternals:
		return tui.SplitRows(r, tui.Fixed(dRes), tui.Fixed(dInt-dRes), tui.Flex(1))
	default:
		// Resources keeps its content, not the rest of the column: a box of
		// blank rows says nothing. What is left stays empty.
		return tui.SplitRows(r, tui.Fixed(dRes), tui.Fixed(min(m.resRows, h-dRes)), tui.Flex(1))
	}
}

// moveDivider applies one drag step to the named divider at pointer (x, y) in
// terminal cells. The row is clamped so no box drops below its frame plus one
// row, and the side width so the main column keeps its minimum. A divider
// sits one row below the band it closes, so the stored row is the pointer row
// plus one.
func (m *topModel) moveDivider(name string, x, y int) {
	if name == "side" {
		m.layout.SideWidth = clampSide(m.contentRect.W, m.contentRect.W-(x-m.contentRect.X))
		return
	}
	if m.layout.Dividers == nil {
		m.layout.Dividers = map[string]int{}
	}
	h := m.contentRect.H
	row := y - m.contentRect.Y + 1
	switch name {
	case "left:proxies":
		m.layout.Dividers[name] = clampRow(row, topMinBoxRows, h-2*topMinBoxRows)
	case "left:events":
		dProxies, _ := m.leftDividerRows(h)
		m.layout.Dividers[name] = clampRow(row, dProxies+topMinBoxRows, h-topMinBoxRows)
	case "side:resources":
		m.layout.Dividers[name] = clampRow(row, topMinBoxRows, m.resourcesDividerMax(h))
	case "side:extender":
		dRes, _, _ := m.sideDividerRows(h)
		m.layout.Dividers[name] = clampRow(row, dRes+topMinBoxRows, h-topMinBoxRows)
	case "side:internals":
		_, dExt, _ := m.sideDividerRows(h)
		m.layout.Dividers[name] = clampRow(row, dExt+topMinBoxRows, h-topMinBoxRows)
	}
}

// isDivider reports whether a region name is a draggable divider.
func isDivider(name string) bool {
	return name == "side" || strings.HasPrefix(name, "left:") || strings.HasPrefix(name, "side:")
}
