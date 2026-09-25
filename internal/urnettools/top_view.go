package urnettools

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// Drawing for `urnet-tools top`. Every function here paints into a tui.Buffer
// from the model alone, so a frame is a pure function of (model, size) and the
// golden tests can compare it as text.

// Panel geometry. The right column is fixed so the numbers in it do not jump
// as the window is resized; the left column takes the rest.
const (
	topSideWidth   = 30
	topProxiesRows = 6 // frame plus four bars
	// topProxiesCompactRows is the Proxies panel for a small pool: frame plus one
	// line. Four bars for one proxy is a lot of box for one number.
	topProxiesCompactRows = 3
	// topProxiesCompactMax is the largest pool drawn as one line.
	topProxiesCompactMax = 3
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
	if !topMin.FitsRect(b.Rect()) {
		m.drawCompact(b)
	} else {
		m.drawFull(b)
	}
	if m.help {
		m.drawHelp(b)
	}
	if m.menu {
		m.drawMenu(b)
	}
}

func (m *topModel) drawFull(b *tui.Buffer) {
	th := m.theme
	rows := tui.SplitRows(b.Rect(), tui.Fixed(1), tui.Flex(1), tui.Fixed(1))
	m.drawHeader(b.Sub(rows[0]))
	m.drawFooter(b.Sub(rows[2]))

	cols := tui.SplitCols(rows[1], tui.Flex(1), tui.Fixed(topSideWidth))
	left := tui.SplitRows(cols[0], tui.Flex(1), tui.Fixed(m.proxiesRows()), tui.Fixed(m.eventsRows()))
	nowRows := topNowRows
	if m.hasTraffic() {
		nowRows = topNowRowsTraffic
	}
	// The side column is Now over Resources; when the provider reports runtime
	// internals and the column is tall enough, Resources shrinks to its three
	// rows and Internals takes the rest.
	resRows := m.resourceRows() + 2
	withInternals := m.hasInternals() && cols[1].H >= nowRows+resRows+topInternalsRows
	var side []tui.Rect
	if withInternals {
		side = tui.SplitRows(cols[1], tui.Fixed(nowRows), tui.Fixed(resRows), tui.Flex(1))
	} else {
		// Resources takes its content, not the rest of the column: a box of blank
		// rows says nothing. What is left stays empty.
		side = tui.SplitRows(cols[1], tui.Fixed(nowRows), tui.Fixed(min(resRows, max(cols[1].H-nowRows, 0))), tui.Flex(1))
	}
	box := func(r tui.Rect, title string) *tui.Buffer {
		return tui.DrawBox(b.Sub(r), title, th.Frame, th.Border, th.Accent, th.ASCII)
	}

	sr := m.series()
	if sr.total != nil && left[0].H >= topTwoGraphsMinRows {
		graphs := tui.SplitRows(left[0], tui.Flex(1), tui.Flex(1))
		m.drawGraph(box(graphs[0], m.throughputTitle("Billable", sr)), sr, sr.billable, sr.scaleB, sr.tailBillable, th.Graph)
		m.drawGraph(box(graphs[1], m.throughputTitle("Total traffic", sr)), sr, sr.total, sr.scaleT, sr.tailTotal, th.Accent)
	} else {
		title := "Throughput"
		if sr.total != nil {
			title = "Billable"
		}
		m.drawGraph(box(left[0], m.throughputTitle(title, sr)), sr, sr.billable, sr.scaleB, sr.tailBillable, th.Graph)
	}
	m.drawProxies(box(left[1], "Proxies"))
	m.drawEvents(box(left[2], "Events"))
	m.drawNow(box(side[0], "Now"))
	m.drawResources(box(side[1], "Resources"))
	if withInternals {
		title := "Internals"
		if m.rt.showing {
			title = "Goroutines"
		}
		m.drawInternals(box(side[2], title))
	}
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
	// verdict and clock keep their place.
	left := h.Put(" urnet-tools top", 0, 0, th.Accent)
	details := "   " + topNodeLabel(m.provider())
	if n := len(m.providers); n > 1 {
		details += fmt.Sprintf(" [%d/%d]", m.cur+1, n)
	}
	if m.snap != nil {
		details += "   " + orDash(m.snap.Version) + "   up " + tui.Duration(time.Duration(m.snap.UptimeSeconds*float64(time.Second)))
	}
	h.Put(tui.Truncate(details, rx-left-1, th.ASCII), left, 0, th.Dim)
}

func (m *topModel) drawFooter(f *tui.Buffer) {
	keys := " q quit"
	if len(m.providers) > 1 {
		keys += "   tab provider"
	}
	keys += "   ? help   m menu"
	if m.trafficOK {
		keys += "   w zoom" // the zoom window is built from the live counters
	}
	if m.rt.ok && m.rt.gOK && m.hasInternals() {
		keys += "   g goroutines"
	}
	keys += "   - " + topIntervalText(m.interval) + " +"
	f.Put(tui.Truncate(keys, f.Width(), m.theme.ASCII), 0, 0, m.theme.Dim)
}

// topIntervalText is 100ms, 250ms, 500ms, 1s, 2s.
func topIntervalText(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return strconv.Itoa(int(d/time.Millisecond)) + "ms"
}

// throughputTitle says how much history the graph is showing. It shows what
// the snapshot carried, not a fixed ten minutes, so a provider that only just
// started does not claim a window it does not have.
func (m *topModel) throughputTitle(name string, sr topSeries) string {
	if sr.zoom {
		title := name + ", last " + tui.Duration(topZoomWindow)
		if len(sr.billable) == 0 {
			title += " (collecting)"
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

// drawGraph draws one rate series. sr.anchor is the absolute index of the newest
// sample (zero when unknown), which keeps completed columns still while the
// graph scrolls; the live rate is the tail, its own newest column. scale holds
// the axis steady: it never sits below what the series needs, and only comes
// down after the peak has stayed low for a while.
func (m *topModel) drawGraph(b *tui.Buffer, sr topSeries, samples []float64, scale *topScale, tail float64, live tui.Style) {
	th := m.theme
	if len(samples) == 0 {
		msg := "waiting for the provider"
		if sr.zoom {
			msg = "collecting live readings"
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
	top := 0.0
	if scale != nil {
		top = scale.top
	}
	// A series that outgrew the held axis (nothing has folded it in yet) still
	// fits: the axis never sits below what is drawn.
	if p := tui.NiceCeil(seriesPeak(samples), true); p > top {
		top = p
	}
	tui.DrawGraph(b, tui.Graph{
		Samples: samples, Binary: true, Format: tui.RateShort, Style: style, AxisStyle: th.Dim,
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
			return topProxiesCompactRows
		}
	}
	return topProxiesRows
}

// eventsRows is the height of the Events panel: its content, at least one row
// (which says there are none) and at most topEventsMax.
func (m *topModel) eventsRows() int {
	return 2 + min(max(len(m.events), topEventsMin), topEventsMax)
}

// drawProxiesCompact is the one-line pool summary: every state with its count,
// a zero dimmed so the states that matter stand out.
func (m *topModel) drawProxiesCompact(b *tui.Buffer) {
	th := m.theme
	p := m.snap.Proxies
	if p.Up+p.Degraded+p.Connecting+p.Dead == 0 {
		b.Put("no proxies: direct only", 0, 0, th.Dim)
		return
	}
	x := 0
	for i, st := range []struct {
		label string
		n     int
		on    tui.Style
	}{{"up", p.Up, th.OK}, {"degraded", p.Degraded, th.Warn}, {"connecting", p.Connecting, tui.Style{}}, {"dead", p.Dead, th.Bad}} {
		style := st.on
		if st.n == 0 {
			style = th.Dim
		}
		if i > 0 {
			x = b.Put("   ", x, 0, tui.Style{})
		}
		x = b.Put(strconv.Itoa(st.n)+" "+st.label, x, 0, style)
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
	p := m.snap.Proxies
	total := p.Up + p.Degraded + p.Connecting + p.Dead
	bars := []struct {
		label string
		n     int
		fill  tui.Style
	}{
		{"up", p.Up, th.OK}, {"degraded", p.Degraded, th.Warn},
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
}

func (m *topModel) drawEvents(b *tui.Buffer) {
	th := m.theme
	now := m.now()
	if len(m.events) == 0 {
		b.Put("no events yet", 0, 0, th.Dim)
		return
	}
	for i, e := range m.sortedEvents() {
		if i >= b.Height() {
			break
		}
		at := e.At.Format("15:04")
		if !sameDay(e.At, now) {
			at = e.At.Format("01-02 15:04")
		}
		x := b.Put(at+" ", 0, i, th.Dim)
		b.Put(tui.Truncate(e.Text, b.Width()-x, th.ASCII), x, i, m.style(e.Level))
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
// line up with the labels of the bars beside them.
func (m *topModel) kvAt(b *tui.Buffer, col, y int, key, value string, valueStyle tui.Style) {
	b.Put(key, 0, y, m.theme.Dim)
	b.Put(tui.Truncate(value, b.Width()-col, m.theme.ASCII), col, y, valueStyle)
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
	row("sessions", fmt.Sprintf("%d pqe / %d cl", s.Sessions.PQE, s.Sessions.Classical), tui.Style{})
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
		vals := make([]uint64, len(r.gor))
		for i, v := range r.gor {
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
	for i, grp := range g.Groups {
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
// pair stays short enough for a bar's value column.
func topBytesPair(used, limit float64) string {
	const mib, gib = 1024 * 1024, 1024 * 1024 * 1024
	if limit >= gib {
		return fmt.Sprintf("%.1f/%.1f GiB", used/gib, limit/gib)
	}
	return fmt.Sprintf("%.0f/%.0f MiB", used/mib, limit/mib)
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
			vals := make([]uint64, len(m.history()))
			for i, v := range m.history() {
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
	"+  -             update slower or faster, down to 100ms",
	"w                zoom the graphs to the last 15 seconds",
	"m                menu: color theme and graph style",
	"g                list where goroutines are parked",
	"billed, moved    billable and total bytes this session",
	"?                show or hide this help",
}

// drawHelp draws the key reference centered over whatever is on screen.
func (m *topModel) drawHelp(b *tui.Buffer) {
	th := m.theme
	w := 0
	for _, l := range helpLines {
		w = max(w, tui.StringWidth(l))
	}
	w += 4
	h := len(helpLines) + 2
	if b.Width() < topHelpMinWidth || b.Height() < h {
		w, h = min(w, b.Width()), min(h, b.Height())
	}
	r := tui.Rect{X: (b.Width() - w) / 2, Y: (b.Height() - h) / 2, W: w, H: h}
	panel := b.Sub(r)
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "Keys", th.Frame, th.Border, th.Accent, th.ASCII)
	for i, l := range helpLines {
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
