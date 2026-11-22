package urnettools

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/connect/internal/tui"
)

// Drawing for `urnet-tools top`. Every function here paints into a tui.Buffer
// from the model alone, so a frame is a pure function of (model, size) and the
// golden tests can compare it as text.

// Panel geometry. The right column is fixed so the numbers in it do not jump
// as the window is resized; the left column takes the rest.
const (
	topSideWidth    = 30
	topSideMaxWidth = 46
	// topProxiesRows is the Proxies panel height: frame plus four bars.
	topProxiesRows = 6
	// topProxiesCompactRows is the Proxies panel for a small pool: frame plus one
	// line. Four bars for one proxy is a lot of box for one number.
	topProxiesCompactRows = 3
	// topProxiesCompactMax is the largest pool drawn as one line.
	topProxiesCompactMax = 3
	// topProxiesGridRows is the total height of the Proxies box when the proxy
	// heatmap grid replaces the bars: frame plus grid rows plus the legend.
	topProxiesGridRows = 15
	// proxyGridMinH is the minimum box height at which the grid is used; below
	// it the bars stay.
	proxyGridMinH = 8
	// proxyGridMinW is the minimum box width at which the grid is used.
	proxyGridMinW = 20
	// topGraphMinRows is the height the throughput graphs must keep even when
	// the Proxies box grows to show the grid.
	topGraphMinRows = 4
	// topEventsMin and topEventsMax bound the Events panel's content rows: it
	// takes what the events need, so a quiet node does not reserve blank rows.
	topEventsMin = 1
	topEventsMax = 4
	topNowRows   = 11
	// topNowRowsTraffic is the Now panel when the provider reports traffic
	// totals: billable and total rates with their averages, the session bytes
	// of each, then the usual rows. 16 so the two interior rows needed to
	// render the state reason fit inside the box.
	topNowRowsTraffic = 16
	// topTwoGraphsMinRows is how tall the throughput area must be to stack a
	// billable graph and a total-traffic graph; shorter shows billable only.
	topTwoGraphsMinRows = 14
	topKeyColumn        = 10
	topHelpMinWidth     = 40
	// topGraphCapacity is the most samples a graph holds: the provider's ten
	// minute ring of per-second samples plus the live column. A fixed capacity
	// keeps the graph's column width still while a young series fills.
	topGraphCapacity = 601
)

// render draws the whole screen into b.
func (m *topModel) render(b *tui.Buffer) {
	m.regions = m.regions[:0]
	m.proxyGridW, m.proxyGridH = 0, 0
	m.compact = false
	if m.overview {
		m.drawOverview(b)
		m.drawNotice(b)
		return
	}
	if !topMin.FitsRect(b.Rect()) {
		// Below the full layout's minimum there is one compact panel and no
		// strip, so the compact view stays on the dashboard.
		m.compact = true
		m.drawCompact(b)
		if m.help {
			m.drawHelp(b)
		}
		if m.menu {
			m.drawMenu(b)
		}
		m.drawNotice(b)
		return
	}
	m.drawFrame(b)
	if m.proxyPopup {
		m.drawProxyPopup(b)
	}
	if m.panel != "" {
		m.drawDetails(b)
	}
	if m.help {
		m.drawHelp(b)
	}
	if m.menu {
		m.drawMenu(b)
	}
	m.drawNotice(b)
}

// topFrameRows splits the full screen into the shared header, the page strip,
// the page content and the shared footer, with the strip assumed.
func topFrameRows(r tui.Rect) (header, strip, content, footer tui.Rect) {
	rows := tui.SplitRows(r, tui.Fixed(1), tui.Fixed(topPageStripRows), tui.Flex(1), tui.Fixed(1))
	return rows[0], rows[1], rows[2], rows[3]
}

// topFrameRowsOnePage splits the full screen when there is only the dashboard
// to show: the content keeps the row the strip would have taken, which is the
// frame exactly as it was before pages existed.
func topFrameRowsOnePage(r tui.Rect) (header, content, footer tui.Rect) {
	rows := tui.SplitRows(r, tui.Fixed(1), tui.Flex(1), tui.Fixed(1))
	return rows[0], rows[1], rows[2]
}

// drawFrame draws the chrome every page shares and then the selected page
// inside the content rectangle it leaves. The page strip takes a row of its
// own on every page, page 1 included, so the pages are visible without
// switching to one. p hides the row for the session and gives it back to the
// content.
func (m *topModel) drawFrame(b *tui.Buffer) {
	header, strip, content, footer := topFrameRows(b.Rect())
	pages, current := m.pageStrip(content)
	showStrip := m.stripShown(b.Rect(), len(pages))
	if !showStrip {
		header, content, footer = topFrameRowsOnePage(b.Rect())
	}
	m.drawHeader(b.Sub(header))
	if showStrip {
		m.drawPageStrip(strip, b, pages, current)
	}
	m.contentRect = content
	m.drawFooter(b.Sub(footer))
	// The footer hints are clickable: record one region per hint that is on
	// the row. While a message holds the row there are no hints to click.
	for _, p := range footerHintPlaces(footer, m.footerHints()) {
		if p.rect.W > 0 {
			m.record(p.hint.name, p.rect)
		}
	}
	m.selectedPage().draw(m, b, content)
	// An operator hovering a divider sees it is draggable.
	m.drawHover(b)
}

// drawDashboard is page 1: the layout every earlier version of top drew. It
// paints inside content and records its hit regions in absolute cells.
func (m *topModel) drawDashboard(b *tui.Buffer, content tui.Rect) {
	th := m.theme
	sideW := m.sideCols(content.W)
	cols := tui.SplitCols(content, tui.Flex(1), tui.Fixed(sideW))
	left := m.splitLeft(cols[0])
	nowRows := topNowRows
	if m.hasTraffic() {
		nowRows = topNowRowsTraffic
	}
	// The side column is Now over Resources; when the provider reports runtime
	// internals and the column is tall enough, Resources shrinks to its three
	// rows and Internals takes the rest.
	resRows := m.resourceRows() + 2
	// The Extender box sits under Resources and ahead of Internals: whether the
	// extender is on and listed is the thing an operator opens top to check, and
	// it is left out (no empty box) for a provider that cannot report it.
	extRows := 0
	if m.hasExtender() {
		extRows = min(m.extenderRows(), max(0, cols[1].H-nowRows-resRows))
	}
	showExtender := extRows >= topMinBoxRows
	if !showExtender {
		extRows = 0
	}
	withInternals := m.hasInternals() && cols[1].H >= nowRows+resRows+extRows+topInternalsRows
	m.nowRows, m.resRows, m.extRows = nowRows, resRows, extRows
	m.showExtender, m.withInternals = showExtender, withInternals
	side := m.splitSide(cols[1])
	box := func(r tui.Rect, name, title string) *tui.Buffer {
		return tui.DrawBox(b.Sub(r), title, th.Frame, th.Border, m.hoverStyle("title:"+name, th.Accent), th.ASCII)
	}

	sr := m.series()
	// [remote chart] The Remote panel takes the bottom of the graph band when
	// it is on and the band can hold both, so the rest of the layout is
	// untouched. See top_remote_panel.go.
	graphArea, remoteArea, showRemote := m.remoteSplit(left[0])
	secondGraphTitle := -1
	if sr.total != nil && graphArea.H >= topTwoGraphsMinRows {
		graphs := tui.SplitRows(graphArea, tui.Flex(1), tui.Flex(1))
		m.drawGraph(box(graphs[0], "graph", m.graphTitle("Billable", sr)), sr, sr.billable, sr.scaleB, sr.tailBillable, th.Graph)
		m.drawGraph(box(graphs[1], "graph", m.graphTitle("Total traffic", sr)), sr, sr.total, sr.scaleT, sr.tailTotal, th.Accent)
		secondGraphTitle = graphs[1].Y
	} else {
		title := "Throughput"
		if sr.total != nil {
			title = "Billable"
		}
		m.drawGraph(box(graphArea, "graph", m.graphTitle(title, sr)), sr, sr.billable, sr.scaleB, sr.tailBillable, th.Graph)
	}
	if showRemote {
		m.drawRemote(box(remoteArea, "remote", "Remote"))
	}
	m.drawProxies(box(left[1], "proxies", "Proxies"))
	m.proxyPopupRect = tui.Rect{X: left[1].X + 1, Y: left[1].Y + 1, W: max(0, left[1].W-2), H: max(0, left[1].H-2)}
	m.drawEvents(box(left[2], "events", "Events"))
	m.drawNow(box(side[0], "now", "Now"))
	m.drawResources(box(side[1], "resources", "Resources"))
	internalsAt := 2
	if showExtender {
		m.drawExtender(box(side[2], "extender", "Extender"))
		internalsAt = 3
	}
	if withInternals {
		title := "Internals"
		if m.rt.showing {
			title = "Goroutines"
		}
		m.drawInternals(box(side[internalsAt], "internals", title))
	}

	// Hit regions: every box, its title row, and the dividers between them.
	m.recordBox("graph", graphArea)
	if showRemote {
		m.recordBox("remote", remoteArea)
	}
	if secondGraphTitle >= 0 {
		m.record("title:graph", tui.Rect{X: left[0].X, Y: secondGraphTitle, W: left[0].W, H: 1})
	}
	m.recordBox("proxies", left[1])
	m.recordBox("events", left[2])
	m.recordBox("now", side[0])
	m.recordBox("resources", side[1])
	if showExtender {
		m.recordBox("extender", side[2])
	}
	if withInternals {
		m.recordBox("internals", side[internalsAt])
	}
	// The vertical divider between the main column and the side column.
	m.record("side", tui.Rect{X: cols[1].X, Y: content.Y, W: 1, H: content.H})
	// The horizontal dividers between the boxes of each column.
	m.record("left:proxies", tui.Rect{X: left[0].X, Y: left[1].Y - 1, W: left[0].W, H: 1})
	m.record("left:events", tui.Rect{X: left[0].X, Y: left[2].Y - 1, W: left[0].W, H: 1})
	m.record("side:resources", tui.Rect{X: side[0].X, Y: side[1].Y - 1, W: side[0].W, H: 1})
	if showExtender {
		m.record("side:extender", tui.Rect{X: side[0].X, Y: side[2].Y - 1, W: side[0].W, H: 1})
	}
	if withInternals {
		m.record("side:internals", tui.Rect{X: side[0].X, Y: side[internalsAt].Y - 1, W: side[0].W, H: 1})
	}
}

// topSideCols sizes the right column from the terminal width: 30 cells up to
// 120 columns so narrow and standard screens keep their layout, then growing
// by half the extra width up to topSideMaxWidth so wide terminals fit IPv6
// addresses and longer values without cutting them.
func topSideCols(width int) int {
	if width <= 120 {
		return topSideWidth
	}
	return min(topSideWidth+(width-120)/2, topSideMaxWidth)
}

// recordBox adds the box and title regions of one drawn panel.
func (m *topModel) recordBox(name string, r tui.Rect) {
	m.record("box:"+name, r)
	m.record("title:"+name, tui.Rect{X: r.X, Y: r.Y, W: r.W, H: 1})
}

func (m *topModel) drawHeader(h *tui.Buffer) {
	th := m.theme
	verdict, lvl := m.verdict()
	flag := ""
	if m.conn == topConnected && m.snap.RestartPending {
		flag = "  RESTART PENDING"
	}
	if m.isSlow() {
		flag += "  SLOW"
		if m.failStreak > 0 {
			flag += " no answer " + tui.Duration(m.now().Sub(m.lastAnswer))
		}
	}
	clock := m.now().Format("15:04:05")
	right := verdict + flag + "    " + clock + " "
	rx := h.Width() - tui.StringWidth(right)
	x := h.Put(verdict, rx, 0, m.style(lvl))
	x = h.Put(flag, x, 0, th.Warn)
	h.Put("    "+clock+" ", x, 0, th.Dim)

	// The node details give way (ellipsis) when the terminal is narrow; the
	// verdict and clock keep their place. The details area is the clickable
	// provider tab, so a click cycles providers.
	left := h.Put(" urnet-tools top", 0, 0, th.Accent)
	if n := len(m.providers); n > 1 {
		m.record("tab:provider", tui.Rect{X: left, Y: 0, W: max(rx-left, 0), H: 1})
	}
	details := "   " + topNodeLabel(m.provider())
	if n := len(m.providers); n > 1 {
		details += fmt.Sprintf(" [%d/%d]", m.cur+1, n)
	}
	if m.snap != nil {
		details += "   " + orDash(m.snap.Version) + "   up " + tui.Duration(time.Duration(m.snap.UptimeSeconds*float64(time.Second)))
	}
	h.Put(tui.Truncate(details, rx-left-1, th.ASCII), left, 0, m.hoverStyle("tab:provider", th.Dim))
}

func (m *topModel) drawFooter(f *tui.Buffer) {
	// A one-line message holds the row while it lasts, so the message is
	// never half a hint.
	if m.noticeActive() {
		return
	}
	places := footerHintPlaces(f.Rect(), m.footerHints())
	end := 0
	for _, p := range places {
		f.Put(p.hint.label, p.rect.X, p.rect.Y, m.hoverStyle(p.hint.name, m.theme.Dim))
		end = p.rect.X + p.rect.W
	}
	// The mode marker takes the right end of the row, and is dropped whole
	// when it cannot sit clear of the last hint.
	if label := m.modeMarker(); label != "" {
		w := tui.StringWidth(label)
		if sx := f.Width() - 1 - w; sx >= end+2 {
			f.Put(label, sx, 0, m.theme.Dim)
		}
	}
}

// drawNotice draws the one-line message on the bottom row of b, in place of
// the footer hints, whichever view is up.
func (m *topModel) drawNotice(b *tui.Buffer) {
	if !m.noticeActive() || b.Height() == 0 {
		return
	}
	row := b.Sub(tui.Rect{Y: b.Height() - 1, W: b.Width(), H: 1})
	row.Fill(row.Rect(), ' ', tui.Style{})
	row.Put(tui.Truncate(" "+m.notice, row.Width(), m.theme.ASCII), 0, 0, m.theme.Accent)
}

// topIntervalText is 100ms, 250ms, 500ms, 1s, 2s.
func topIntervalText(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return strconv.Itoa(int(d/time.Millisecond)) + "ms"
}

// throughputTitle says how much history the graph is showing. A graph window
// names its fixed level, and while the readings do not reach the window yet it
// says how much they do cover, so a young top never claims a window it has not
// filled.
func (m *topModel) throughputTitle(name string, sr topSeries) string {
	if sr.window {
		title := name + ", " + topGraphWindowNames[sr.level]
		if !sr.full {
			if sr.covered < time.Second {
				title += " (collecting)"
			} else {
				title += " (collecting " + tui.Duration(sr.covered) + ")"
			}
		}
		if m.conn == topDisconnected {
			title += " (stale)"
		}
		return title
	}
	if m.windowHours > 0 {
		title := name + ", " + windowTitle(m.windowHours)
		if m.conn == topDisconnected {
			title += " (stale)"
		}
		return title
	}
	h := m.history()
	if len(h) == 0 {
		return name
	}
	step := 1.0
	if m.snap.Rate.HistoryIntervalSeconds > 0 {
		step = m.snap.Rate.HistoryIntervalSeconds
	}
	title := name + ", " + tui.Duration(time.Duration(float64(len(h))*step*float64(time.Second)))
	if m.conn == topDisconnected {
		title += " (stale)"
	}
	return title
}

// graphColumns matches DrawGraph's axis and symbol geometry.
func (m *topModel) graphColumns(width int, samples []float64, scale *topScale) int {
	top := 0.0
	if scale != nil {
		top = scale.top
	}
	if p := tui.NiceCeil(seriesPeak(samples), !tui.DecimalUnits()); p > top {
		top = p
	}
	axis := tui.StringWidth(tui.RateShort(0))
	if top > 0 {
		axis = max(axis, tui.StringWidth(tui.RateShort(top)))
	}
	plot := width - axis - 1
	if plot < 1 {
		plot = width
	}
	perCell := 2
	if m.graph == tui.GraphBlock && !m.theme.ASCII {
		perCell = 1
	}
	return max(0, plot*perCell)
}

func (m *topModel) graphTitle(name string, sr topSeries) string {
	return m.throughputTitle(name, sr)
}

// drawGraph draws one rate series. sr.anchor is the absolute index of the newest
// sample (zero when unknown), which keeps completed columns still while the
// graph scrolls; a window series uses it as the newest reading time instead.
// The live rate is the tail, its own newest column. scale holds
// the axis steady: it never sits below what the series needs, and only comes
// down after the peak has stayed low for a while.
func (m *topModel) drawGraph(b *tui.Buffer, sr topSeries, samples []float64, scale *topScale, tail float64, live tui.Style) {
	th := m.theme
	if len(samples) == 0 {
		msg := "waiting for the provider"
		if sr.window {
			msg = "collecting readings"
		}
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		b.Put(tui.Truncate(msg, b.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	style := live
	if m.conn == topDisconnected {
		style = th.Dim
	}
	if sr.window {
		// A window is placed on a time axis of the plot's own width: readings
		// become step bars when they are fewer than the columns, and the
		// highest reading in each column wins when they are more.
		n := m.graphColumns(b.Width(), samples, scale)
		samples = spreadSeries(sr.at, sr.spans, samples, sr.anchor, topGraphWindows[sr.level], n)
		sr.capacity, sr.live = len(samples), false
	} else if sr.columns {
		n := min(m.graphColumns(b.Width(), samples, scale), len(samples))
		samples = samples[len(samples)-n:]
		sr.anchor, sr.capacity = 0, len(samples)
	}
	// The axis follows what is drawn. The peak is taken over the columns on
	// screen and the live value, not over the whole retained history, so a
	// burst that has scrolled off the left edge no longer holds the axis high.
	// The held scale still rises at once and comes down only after the peak
	// has stayed low for a while.
	// The axis follows what is drawn: the peak over the columns on screen and
	// the live value. A burst on screen shows at its real height. Once it has
	// scrolled off the left edge the held scale comes down to the next level
	// that fits what is left.
	peak := seriesPeak(samples)
	if sr.live && tail > peak {
		peak = tail
	}
	top := 0.0
	if scale != nil {
		scale.update(peak, m.now(), !tui.DecimalUnits())
		top = scale.top
	}
	if p := tui.NiceCeil(peak, !tui.DecimalUnits()); p > top {
		top = p
	}
	tui.DrawGraph(b, tui.Graph{
		Samples: samples, Binary: !tui.DecimalUnits(), Format: tui.RateShort, Style: style, AxisStyle: th.Dim,
		Anchor: sr.anchor, Capacity: sr.capacity, Tail: tail, HasTail: sr.live, Max: top, Symbols: m.graph,
	}, th.ASCII)
}

// hasTraffic reports whether the provider sends billable-versus-total figures.
func (m *topModel) hasTraffic() bool {
	return m.snap != nil && m.snap.Traffic != nil
}

// proxiesRows is the height of the Proxies panel: one line for a small pool
// (a direct-only node has none or one), four bars for a real one. The rows this
// gives back go to the graphs.
func (m *topModel) proxiesRows() int {
	if m.snap != nil {
		p := m.snap.Proxies
		if p.Up+p.Degraded+p.Connecting+p.Dead <= topProxiesCompactMax {
			if p.UDP != nil {
				return topProxiesCompactRows + 1
			}
			return topProxiesCompactRows
		}
		if p.UDP != nil {
			return topProxiesRows + 2
		}
	}
	return topProxiesRows
}

// hasProxyGridData reports whether the provider answered proxy_cells with a
// non-empty grid. Without it the heatmap cannot draw and the panel keeps the
// bars (a provider that predates the command, or a direct-only node).
func (m *topModel) hasProxyGridData() bool {
	return len(m.proxyCells) > 0
}

// proxiesBoxMode reports how the Proxies box of h rows by w cols should be
// drawn: proxyBoxGrid (compact legend plus the heatmap grid) or proxyBoxBars
// (the classic four bars). The box must be tall and wide enough for the grid
// to read; anything smaller keeps the bars.
func proxiesBoxMode(h, w int) int {
	if h >= proxyGridMinH && w >= proxyGridMinW {
		return proxyBoxGrid
	}
	return proxyBoxBars
}

// The two Proxies panel modes.
const (
	proxyBoxBars = iota
	proxyBoxGrid
)

// eventsRows is the height of the Events panel: its content, at least one row
// (which says there are none) and at most topEventsMax. The count is after
// collapsing consecutive repeats, which is what the panel draws.
func (m *topModel) eventsRows() int {
	vs := collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter))
	return 2 + min(max(len(vs), topEventsMin), topEventsMax)
}

// drawUDPProxyCoverage draws the UDP coverage line and optional percentage line
// starting at row y of b. It returns the number of rows drawn.
// States with count > 0 are shown; unchecked is always shown.
func (m *topModel) drawUDPProxyCoverage(b *tui.Buffer, y int) int {
	th := m.theme
	udp := m.snap.Proxies.UDP
	if udp == nil {
		return 0
	}
	checked := udp.OK + udp.NoRelay + udp.Mismatch + udp.Unknown
	online := checked + udp.Unchecked

	// Build the main line with styled segments. Every state with a count is
	// shown, and unchecked always is. A line wider than the panel first uses the
	// short labels, then drops the least important states, so ok and unchecked
	// keep their counts and labels.
	b.Put("udp ", 0, y, th.Dim)
	x := utf8.RuneCountInString("udp ")
	type udpSeg struct {
		text  string
		style tui.Style
		keep  bool // never dropped to make room
	}
	build := func(short bool) []udpSeg {
		noRelay, mismatch, unknown := "no UDP", "mismatch", "unknown"
		if short {
			noRelay, mismatch, unknown = "no-udp", "diff", "unk"
		}
		var segs []udpSeg
		// A state with no count is left out, except unchecked, which is always
		// drawn. ok and unchecked are kept when there is not enough room.
		add := func(n int, label string, style tui.Style, always, keep bool) {
			if n == 0 && !always {
				return
			}
			segs = append(segs, udpSeg{strconv.Itoa(n) + " " + label, style, keep})
		}
		add(udp.OK, "ok", th.OK, false, true)
		add(udp.NoRelay, noRelay, th.Warn, false, false)
		add(udp.Mismatch, mismatch, th.Warn, false, false)
		add(udp.Unknown, unknown, th.Dim, false, false)
		add(udp.Unchecked, "unchecked", th.Dim, true, true)
		return segs
	}
	gap := 1
	if b.Width() >= 60 {
		gap = 2
	}
	room := b.Width() - x
	width := func(segs []udpSeg) int {
		w := 0
		for i, sg := range segs {
			if i > 0 {
				w += gap
			}
			w += utf8.RuneCountInString(sg.text)
		}
		return w
	}
	segs := build(b.Width() < 60)
	if width(segs) > room {
		segs = build(true)
	}
	// Drop the least important states from the right until it fits.
	for width(segs) > room {
		drop := -1
		for i := len(segs) - 1; i >= 0; i-- {
			if !segs[i].keep {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		segs = append(segs[:drop], segs[drop+1:]...)
	}
	for i, sg := range segs {
		if i > 0 {
			x += gap
		}
		b.Put(sg.text, x, y, sg.style)
		x += utf8.RuneCountInString(sg.text)
	}

	// Percentage line: only when there is room.
	rows := 1
	if y+1 < b.Height() {
		var pctLine string
		switch {
		case checked > 0 && online > 0:
			pctLine = fmt.Sprintf("%d%% of checked, %d%% of online",
				udp.OK*100/checked, udp.OK*100/online)
		case checked > 0:
			pctLine = fmt.Sprintf("%d%% of checked, n/a of online",
				udp.OK*100/checked)
		case checked == 0 && online > 0:
			pctLine = "n/a of checked, 0% of online"
		default:
			pctLine = "n/a of checked, n/a of online"
		}
		b.Put(pctLine, 0, y+1, th.Dim)
		rows = 2
	}
	return rows
}

// drawProxiesCompact is the one-line pool summary: every state with its count,
// a zero dimmed so the states that matter stand out.
func (m *topModel) drawProxiesCompact(b *tui.Buffer) {
	th := m.theme
	p := m.snap.Proxies
	if p.Up+p.Degraded+p.Connecting+p.Dead == 0 {
		b.Put("no proxies: direct only", 0, 0, th.Dim)
		if b.Height() >= 2 {
			m.drawUDPProxyCoverage(b, 1)
		}
		return
	}
	x := 0
	for i, st := range []struct {
		label string
		n     int
		on    tui.Style
	}{{"up", p.Up, th.OK}, {"dropped", p.Degraded, th.Warn}, {"connecting", p.Connecting, tui.Style{}}, {"dead", p.Dead, th.Bad}} {
		style := st.on
		if st.n == 0 {
			style = th.Dim
		}
		if i > 0 {
			x = b.Put("   ", x, 0, tui.Style{})
		}
		x = b.Put(strconv.Itoa(st.n)+" "+st.label, x, 0, style)
	}
	// UDP coverage: add below when there is room (height >= 2).
	if b.Height() >= 2 {
		m.drawUDPProxyCoverage(b, 1)
	}
}

func (m *topModel) drawProxies(b *tui.Buffer) {
	th := m.theme
	if m.snap == nil {
		b.Put("no data", 0, 0, th.Dim)
		return
	}
	if b.Height() < topProxiesRows-2 {
		m.drawProxiesCompact(b)
		return
	}
	// Grid mode: the provider feeds proxy_cells and the box is tall enough.
	gridH := b.Height()
	if m.snap.Proxies.UDP != nil {
		gridH -= 2
	}
	if m.hasProxyGridData() && proxiesBoxMode(gridH, b.Width()) == proxyBoxGrid {
		if m.snap.Proxies.UDP == nil {
			m.drawProxyHeatmap(b)
		} else {
			m.drawProxyHeatmap(b.Sub(tui.Rect{W: b.Width(), H: gridH}))
			m.drawUDPProxyCoverage(b, gridH)
		}
		return
	}
	p := m.snap.Proxies
	total := p.Up + p.Degraded + p.Connecting + p.Dead
	bars := []struct {
		label string
		n     int
		fill  tui.Style
	}{
		{"up", p.Up, th.OK}, {"dropped", p.Degraded, th.Warn},
		{"connecting", p.Connecting, th.Dim}, {"dead", p.Dead, th.Bad},
	}
	for i, bar := range bars {
		frac := 0.0
		if total > 0 {
			frac = float64(bar.n) / float64(total)
		}
		tui.DrawBar(b.Sub(tui.Rect{Y: i, W: b.Width(), H: 1}), tui.Bar{
			Label: bar.label, LabelWidth: 10, Value: strconv.Itoa(bar.n), Frac: frac,
			LabelStyle: th.Dim, Fill: bar.fill, Empty: th.Dim,
		}, th.ASCII)
	}
	// UDP coverage: draw below the bars when there is room.
	if b.Height() > 4 {
		m.drawUDPProxyCoverage(b, 4)
	}
	// Legend row: non-Glow heatmap styles show the style name.
	if m.proxyHeatmapStyle != heatmapGlow && gridH > 1 {
		b.Put(m.proxyHeatmapStyle.String(), 0, gridH, th.Dim)
	}
}

func (m *topModel) drawEvents(b *tui.Buffer) {
	th := m.theme
	now := m.now()
	m.eventHeight = b.Height()
	if len(m.events) == 0 {
		b.Put("no events yet", 0, 0, th.Dim)
		return
	}
	views := collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter))
	if len(views) == 0 {
		b.Put("no events match filter", 0, 0, th.Dim)
		m.eventScroll = 0
		return
	}
	m.eventScroll = min(m.eventScroll, max(0, len(views)-b.Height()))
	start := min(m.eventScroll, max(0, len(views)-b.Height()))
	for i, v := range views {
		if i < start {
			continue
		}
		if i-start >= b.Height() {
			break
		}
		y := i - start
		at := eventTime(v.At, now, m.eventZoom)
		x := b.Put(at+" ", 0, y, th.Dim)
		x = b.Put(string(eventIcon(v.Level, th))+" ", x, y, m.style(v.Level))
		text := v.Text
		if v.Count > 1 {
			text += "  x" + strconv.Itoa(v.Count)
		}
		b.Put(tui.Truncate(text, b.Width()-x, th.ASCII), x, y, m.style(v.Level))
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// kv draws "key   value" on row y with a fixed key column.
func (m *topModel) kv(b *tui.Buffer, y int, key, value string, valueStyle tui.Style) {
	m.kvAt(b, topKeyColumn, y, key, value, valueStyle)
}

// kvAt is kv with the value starting at column col, so a panel's plain rows
// line up with the labels of the bars beside them. Long values keep both ends
// and join them with an ellipsis so addresses and units stay readable.
func (m *topModel) kvAt(b *tui.Buffer, col, y int, key, value string, valueStyle tui.Style) {
	b.Put(key, 0, y, m.theme.Dim)
	b.Put(tui.TruncateMiddle(value, b.Width()-col, m.theme.ASCII), col, y, valueStyle)
}

func (m *topModel) drawNow(b *tui.Buffer) {
	th := m.theme
	s := m.snap
	if s == nil {
		b.Put("no data", 0, 0, th.Dim)
		return
	}
	stale := m.conn == topDisconnected
	rates := m.rates()
	rateStyle := func(v float64) tui.Style {
		if stale || v <= 0 {
			return th.Dim
		}
		return th.OK
	}
	y := 0
	row := func(key, value string, style tui.Style) {
		m.kv(b, y, key, value, style)
		y++
	}
	if tr := s.Traffic; tr != nil {
		// Billable is what earns; total is everything moved. Session bytes count
		// from provider start.
		row("billable", tui.Rate(rates.billable), rateStyle(rates.billable))
		row(" 1m avg", tui.Rate(s.Rate.Avg1mBps), tui.Style{})
		row(" 5m avg", tui.Rate(s.Rate.Avg5mBps), tui.Style{})
		row("total", tui.Rate(rates.total), rateStyle(rates.total))
		row(" 1m avg", tui.Rate(tr.TotalAvg1mBps), tui.Style{})
		row(" 5m avg", tui.Rate(tr.TotalAvg5mBps), tui.Style{})
		row("billed", tui.Bytes(float64(tr.BillableBytes)), tui.Style{})
		row("moved", tui.Bytes(float64(tr.TotalBytes)), tui.Style{})
	} else {
		row("current", tui.Rate(rates.billable), rateStyle(rates.billable))
		row("1m avg", tui.Rate(s.Rate.Avg1mBps), tui.Style{})
		row("5m avg", tui.Rate(s.Rate.Avg5mBps), tui.Style{})
	}
	row("clients", strconv.Itoa(s.Clients), tui.Style{})
	row("sessions", fmt.Sprintf("%d pqe / %d cl", s.Sessions.PQE, classicalSessions(s.Clients, s.Sessions.PQE, s.Sessions.Classical)), tui.Style{})
	tui.DrawBar(b.Sub(tui.Rect{Y: y, W: b.Width(), H: 1}), tui.Bar{
		Label: "pressure", LabelWidth: topKeyColumn - 1, Value: fmt.Sprintf("%.2f", s.Pressure), Frac: s.Pressure,
		LabelStyle: th.Dim, Fill: th.Level(s.Pressure, 0.7, 0.9), Empty: th.Dim,
	}, th.ASCII)
	y++
	if s.RestartPending {
		b.Put("RESTART PENDING", 0, y, th.Warn)
	}
	y++ // the row stays reserved so the reason below does not jump when it appears
	// The reason can be a whole sentence; wrap it onto the rows left.
	if _, text, ok := s.whyRow(); ok {
		for i, ln := range wrapText(text, b.Width(), 2, th.ASCII) {
			b.Put(ln, 0, y+i, th.Warn)
		}
	}
}

// resourceRows is how many rows drawResources fills, so its box can fit them.
// It mirrors drawResources's conditions, one row per figure the platform gave.
func (m *topModel) resourceRows() int {
	if m.snap == nil {
		return 1 // "no data"
	}
	r := m.snap.Resources
	n := 0
	if (r.MemLimitBytes != nil && *r.MemLimitBytes > 0) || r.HeapInuseBytes > 0 {
		n++
	}
	if r.OpenFDs != nil {
		n++
	}
	if r.RSSBytes != nil {
		n++
	}
	if r.Goroutines > 0 && !m.hasInternals() {
		n++
	}
	return max(n, 1)
}

func (m *topModel) drawResources(b *tui.Buffer) {
	th := m.theme
	if m.snap == nil {
		b.Put("no data", 0, 0, th.Dim)
		return
	}
	r := m.snap.Resources
	// Bars and plain rows share one label column, sized for "heap" plus a gap.
	const col = 6
	bar := func(y int, label, value string, frac float64) {
		tui.DrawBar(b.Sub(tui.Rect{Y: y, W: b.Width(), H: 1}), tui.Bar{
			Label: label, LabelWidth: col - 1, Value: value, Frac: frac,
			LabelStyle: th.Dim, Fill: th.Level(frac, 0.7, 0.9), Empty: th.Dim,
		}, th.ASCII)
	}
	y := 0
	// Fields the platform could not supply are left out, not drawn as zero.
	switch {
	case r.MemLimitBytes != nil && *r.MemLimitBytes > 0:
		bar(y, "heap", topBytesPair(float64(r.HeapInuseBytes), float64(*r.MemLimitBytes)),
			float64(r.HeapInuseBytes)/float64(*r.MemLimitBytes))
		y++
	case r.HeapInuseBytes > 0:
		m.kvAt(b, col, y, "heap", tui.Bytes(float64(r.HeapInuseBytes)), tui.Style{})
		y++
	}
	switch {
	case r.OpenFDs != nil && r.FDLimit != nil && *r.FDLimit > 0:
		bar(y, "fds", fmt.Sprintf("%d/%d", *r.OpenFDs, *r.FDLimit), float64(*r.OpenFDs)/float64(*r.FDLimit))
		y++
	case r.OpenFDs != nil:
		m.kvAt(b, col, y, "fds", strconv.Itoa(*r.OpenFDs), tui.Style{})
		y++
	}
	if r.RSSBytes != nil {
		m.kvAt(b, col, y, "rss", tui.Bytes(float64(*r.RSSBytes)), tui.Style{})
		y++
	}
	if r.Goroutines > 0 && !m.hasInternals() {
		m.kvAt(b, col, y, "gor", strconv.Itoa(r.Goroutines), tui.Style{})
	}
}

// drawInternals is the runtime panel: what the Go runtime says about the
// provider process. Goroutines lead, with their trend; the rest is heap, GC and
// scheduler figures. With g pressed it lists where the goroutines are parked.
func (m *topModel) drawInternals(b *tui.Buffer) {
	th := m.theme
	r := &m.rt
	if r.showing {
		m.drawGoroutineGroups(b)
		return
	}
	in := r.cur
	if in == nil {
		b.Put("no data", 0, 0, th.Dim)
		return
	}
	const col = 8
	y := 0
	row := func(key, value string, style tui.Style) {
		m.kvAt(b, col, y, key, value, style)
		y++
	}
	// The trend fits what is left of the row after the count.
	gor := strconv.FormatUint(in.Goroutines, 10)
	m.kvAt(b, col, y, "gor", gor, tui.Style{})
	if room := b.Width() - col - len(gor) - 1; room >= 4 && len(r.gor) > 1 {
		// Scaled from the window's own minimum: a count that moves between 1000
		// and 1170 would otherwise read as a flat line at the top.
		lo := r.gor[0]
		for _, v := range r.gor {
			lo = min(lo, v)
		}
		history := r.gor
		if m.interval <= time.Second && len(history) > min(room, 12) {
			history = history[len(history)-min(room, 12):]
		}
		vals := make([]uint64, len(history))
		for i, v := range history {
			vals[i] = uint64(max(v-lo, 0))
		}
		b.Put(tui.Spark(vals, min(room, 12), th.ASCII), col+len(gor)+1, y, th.Graph)
	}
	y++
	row("objects", tui.Bytes(float64(in.HeapObjectsBytes)), tui.Style{})
	row("stacks", tui.Bytes(float64(in.HeapStacksBytes)), tui.Style{})
	if in.HeapGoalBytes > 0 {
		row("goal", tui.Bytes(float64(in.HeapGoalBytes)), th.Dim)
	}
	if v, ok := r.internalsRate(func(n *NodeInternals) uint64 { return n.AllocBytes }); ok {
		row("alloc", tui.Rate(v), tui.Style{})
	}
	// The pause and latency quantiles are only figures once a full window
	// completes; before that the provider reports IntervalSeconds 0 and p99
	// 0.0, which would read as a real measurement. Show a placeholder instead.
	ready := in.IntervalSeconds > 0
	if v, ok := r.internalsRate(func(n *NodeInternals) uint64 { return n.GCCycles }); ok {
		p99 := "--"
		if ready {
			p99 = topMillis(in.GCPauseP99Ms)
		}
		row("gc", fmt.Sprintf("%.0f/min p99 %s", v*60, p99), tui.Style{})
	}
	row("gc cpu", fmt.Sprintf("%.1f%%", in.GCCPUFraction*100), m.style(topLevelFor(in.GCCPUFraction, 0.10, 0.25)))
	sched := "--"
	if ready {
		sched = topMillis(in.SchedLatP99Ms)
	}
	row("sched", "p99 "+sched, m.style(topLevelFor(in.SchedLatP99Ms, 10, 50)))
	// The transport split, once the provider has run H3. Both rows are drawn
	// only when the box that is left can hold them: on a terminal where the
	// panel fits its runtime figures but not these two, the panel keeps the
	// figures instead of vanishing whole.
	if t := in.Transport; t != nil && y+2 <= b.Height() {
		share := "--"
		if t.H3TxSharePercent >= 0 {
			share = fmt.Sprintf("%d%%", t.H3TxSharePercent)
		}
		// Colour on the state an operator can act on. Drops are cumulative for
		// the life of the process, so thresholding them would latch this row
		// yellow after a single drop ever while H3 sat connected, and would show
		// nothing for H3 sitting down with a clean drop count.
		level := topInfo
		if t.H3Up <= 0 {
			level = topWarn
		}
		row("h3", fmt.Sprintf("up %d tx %s drops %d", t.H3Up, share, t.H3Drops), m.style(level))
		row("h3/all", topBytePair(t.H3BytesTx, t.TxBytesTotal()), th.Dim)
	}
}

// topBytePair renders "one/all" in one unit, e.g. "12.3/57.0 GiB". The h3/all
// row reads it as how much of all outbound payload one mode carried. The unit
// follows the larger side so a long-running node stays inside the panel at TiB
// scale instead of truncating the unit off the end of the value.
func topBytePair(one, all uint64) string {
	unit, scale := "B", float64(1)
	switch {
	case all >= 1<<40:
		unit, scale = "TiB", 1<<40
	case all >= 1<<30:
		unit, scale = "GiB", 1<<30
	case all >= 1<<20:
		unit, scale = "MiB", 1<<20
	case all >= 1<<10:
		unit, scale = "KiB", 1<<10
	}
	return fmt.Sprintf("%.1f/%.1f %s", float64(one)/scale, float64(all)/scale, unit)
}

// topLevelFor is topWarn past warn and topBad past bad, else no color.
func topLevelFor(v, warn, bad float64) topLevel {
	switch {
	case v >= bad:
		return topBad
	case v >= warn:
		return topWarn
	}
	return topInfo
}

// topMillis is "0.4ms", "12ms" or "1.2s".
func topMillis(ms float64) string {
	switch {
	case ms >= 1000:
		return fmt.Sprintf("%.1fs", ms/1000)
	case ms >= 10:
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fms", ms)
}

// drawGoroutineGroups lists where the goroutines are parked, largest first.
func (m *topModel) drawGoroutineGroups(b *tui.Buffer) {
	th := m.theme
	g := m.rt.groups
	if g == nil {
		b.Put("collecting the goroutine profile", 0, 0, th.Dim)
		return
	}
	b.Put(tui.Truncate(fmt.Sprintf("%d total", g.Total), b.Width(), th.ASCII), 0, 0, th.Dim)
	const countW = 7
	// The wheel moves the window down the list; the largest group stays on top.
	m.gorHeight = max(b.Height()-1, 0)
	m.gorScroll = min(m.gorScroll, max(0, len(g.Groups)-m.gorHeight))
	start := m.gorScroll
	for i, grp := range g.Groups[start:] {
		y := i + 1
		if y >= b.Height() {
			break
		}
		cnt := strconv.Itoa(grp.Count)
		b.Put(cnt, max(countW-len(cnt), 0), y, tui.Style{})
		b.Put(tui.Truncate(shortFunc(grp.Func), b.Width()-countW-1, th.ASCII), countW+1, y, th.Dim)
	}
}

// topBytesPair is "1.8/4.0 GiB": both numbers in the unit of the limit, so the
// pair stays short enough for a bar's value column. The unit follows the
// binary or decimal choice of the u key.
func topBytesPair(used, limit float64) string {
	base, mega, giga := tui.UnitBase(), "MiB", "GiB"
	if tui.DecimalUnits() {
		mega, giga = "MB", "GB"
	}
	if limit >= base*base*base {
		return fmt.Sprintf("%.1f/%.1f %s", used/(base*base*base), limit/(base*base*base), giga)
	}
	return fmt.Sprintf("%.0f/%.0f %s", used/(base*base), limit/(base*base), mega)
}

// drawCompact is the single-panel view for terminals below topMin.
func (m *topModel) drawCompact(b *tui.Buffer) {
	th := m.theme
	in := tui.DrawBox(b, "urnet-tools top", th.Frame, th.Border, th.Accent, th.ASCII)
	if in.Height() == 0 {
		return
	}
	verdict, lvl := m.verdict()
	x := in.Put(tui.Truncate(verdict, in.Width(), th.ASCII), 0, 0, m.style(lvl))
	if m.snap != nil && in.Height() > 1 {
		in.Put(" "+tui.Rate(m.rates().billable), x, 0, tui.Style{})
		p := m.snap.Proxies
		in.Put(tui.Truncate(fmt.Sprintf("clients %d  up %d/%d", m.snap.Clients, p.Up, p.Up+p.Degraded+p.Connecting+p.Dead), in.Width(), th.ASCII), 0, 1, th.Dim)
		if in.Height() > 2 {
			history := m.history()
			if len(m.trafficHistory.billable) > 0 {
				history = m.trafficHistory.billable
			}
			if len(history) > in.Width() {
				history = history[len(history)-in.Width():]
			}
			vals := make([]uint64, len(history))
			for i, v := range history {
				vals[i] = uint64(max(v, 0))
			}
			in.Put(tui.Spark(vals, in.Width(), th.ASCII), 0, 2, th.Graph)
		}
	}
	if in.Height() > 3 {
		in.Put(tui.Truncate("q quit, enlarge for the full view", in.Width(), th.ASCII), 0, in.Height()-1, th.Dim)
	}
}

// helpLines is the key reference the ? overlay shows.
var helpLines = []string{
	"q, Esc, Ctrl-C   quit",
	"Tab, Shift-Tab   next or previous provider",
	"]  [             next or previous page",
	"1 - 9            go to a page",
	"p                show or hide the page tabs",
	"+  -             change refresh interval, down to 100ms",
	"w  W             graph time scale: 10s, 1m, 5m, 15m",
	"r                show or hide the mirrored Remote transfer chart",
	"u                switch units between GiB (binary) and GB (decimal)",
	"m                menu: color theme, graph style and heatmap style",
	"K                mouse mode, or keyboard mode to select text",
	"c                copy the screen to the clipboard",
	"t                cycle graph history: 10m, 1h, 24h",
	"o                show all providers",
	"a / i / v        alerts / resource gauges / carriers",
	"g                list where goroutines are parked",
	"billed, moved    billable and total bytes this session",
	"?                show or hide this help",
	"Grid state: up green, dropped yellow, connecting blue, dead red.",
	"Parked is dim. Green brightness is throughput.",
	"A bright dot fades after recent traffic. proxy.afterglow_seconds sets the time.",
	"One dot is one proxy. A cell has upper and lower dots.",
	"Groups show the worst state. Hover or click for addresses.",
	"Mono uses blocks. Colorblind uses blocks and the worst state style.",
	"Colorblind uses bold and dim. Hover for each state.",
	"ASCII uses symbols for state and activity.",
	"Heatmap styles: Glow, Aurora, Sonar, Locality, Blocks, Timeline.",
	"The page tabs need a 20 row terminal. Smaller screens keep the compact view.",
}

// helpText is the key reference the ? overlay shows, less the copy line when
// this view has no clipboard to copy to.
func (m *topModel) helpText() []string {
	if m.clip != nil {
		return helpLines
	}
	out := make([]string, 0, len(helpLines))
	for _, l := range helpLines {
		if strings.HasPrefix(l, "c ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// drawHelp draws the key reference centered over whatever is on screen.
func (m *topModel) drawHelp(b *tui.Buffer) {
	lines := m.helpText()
	th := m.theme
	w := 0
	for _, l := range lines {
		w = max(w, tui.StringWidth(l))
	}
	w += 4
	h := len(lines) + 2
	if b.Width() < topHelpMinWidth || b.Height() < h {
		w, h = min(w, b.Width()), min(h, b.Height())
	}
	r := tui.Rect{X: (b.Width() - w) / 2, Y: (b.Height() - h) / 2, W: w, H: h}
	panel := b.Sub(r)
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "Keys", th.Frame, th.Border, th.Accent, th.ASCII)
	for i, l := range lines {
		in.Put(tui.Truncate(" "+l, in.Width(), th.ASCII), 0, i, tui.Style{})
	}
}

// wrapText breaks s into at most maxLines lines no wider than width, on word
// boundaries where it can. What does not fit is cut with an ellipsis on the
// last line.
func wrapText(s string, width, maxLines int, ascii bool) []string {
	if width < 1 || maxLines < 1 {
		return nil
	}
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		next := word
		if cur != "" {
			next = cur + " " + word
		}
		if tui.StringWidth(next) <= width {
			cur = next
			continue
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		cur = word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = tui.Truncate(lines[maxLines-1]+" ...", width, ascii)
	}
	for i, l := range lines {
		lines[i] = tui.Truncate(l, width, ascii)
	}
	return lines
}
