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
	topSideWidth    = 30
	topProxiesRows  = 6 // frame plus four bars
	topEventsRows   = 6 // frame plus four events
	topNowRows      = 11
	topKeyColumn    = 10
	topHelpMinWidth = 40
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
}

func (m *topModel) drawFull(b *tui.Buffer) {
	th := m.theme
	rows := tui.SplitRows(b.Rect(), tui.Fixed(1), tui.Flex(1), tui.Fixed(1))
	m.drawHeader(b.Sub(rows[0]))
	m.drawFooter(b.Sub(rows[2]))

	cols := tui.SplitCols(rows[1], tui.Flex(1), tui.Fixed(topSideWidth))
	left := tui.SplitRows(cols[0], tui.Flex(1), tui.Fixed(topProxiesRows), tui.Fixed(topEventsRows))
	side := tui.SplitRows(cols[1], tui.Fixed(topNowRows), tui.Flex(1))
	box := func(r tui.Rect, title string) *tui.Buffer {
		return tui.DrawBox(b.Sub(r), title, th.Frame, th.Border, th.Accent, th.ASCII)
	}

	m.drawThroughput(box(left[0], m.throughputTitle()))
	m.drawProxies(box(left[1], "Proxies"))
	m.drawEvents(box(left[2], "Events"))
	m.drawNow(box(side[0], "Now"))
	m.drawResources(box(side[1], "Resources"))
}

func (m *topModel) drawHeader(h *tui.Buffer) {
	th := m.theme
	verdict, lvl := m.verdict()
	flag := ""
	if m.conn == topConnected && m.snap.RestartPending {
		flag = "  RESTART PENDING"
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
	keys += "   ? help   +/- rate " + topIntervalText(m.interval)
	f.Put(tui.Truncate(keys, f.Width(), m.theme.ASCII), 0, 0, m.theme.Dim)
}

// topIntervalText is 250ms, 500ms, 1s, 2s.
func topIntervalText(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return strconv.Itoa(int(d/time.Millisecond)) + "ms"
}

// throughputTitle says how much history the graph is showing. It shows what
// the snapshot carried, not a fixed ten minutes, so a provider that only just
// started does not claim a window it does not have.
func (m *topModel) throughputTitle() string {
	h := m.history()
	if len(h) == 0 {
		return "Throughput"
	}
	step := 1.0
	if m.snap.Rate.HistoryIntervalSeconds > 0 {
		step = m.snap.Rate.HistoryIntervalSeconds
	}
	title := "Throughput, " + tui.Duration(time.Duration(float64(len(h))*step*float64(time.Second)))
	if m.conn == topDisconnected {
		title += " (stale)"
	}
	return title
}

func (m *topModel) drawThroughput(b *tui.Buffer) {
	th := m.theme
	h := m.history()
	if len(h) == 0 {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		b.Put(tui.Truncate(msg, b.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	style := th.Graph
	if m.conn == topDisconnected {
		style = th.Dim
	}
	tui.DrawGraph(b, tui.Graph{
		Samples: h, Binary: true, Format: tui.RateShort, Style: style, AxisStyle: th.Dim,
	}, th.ASCII)
}

func (m *topModel) drawProxies(b *tui.Buffer) {
	th := m.theme
	if m.snap == nil {
		b.Put("no data", 0, 0, th.Dim)
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
	current := th.OK
	if stale || s.Rate.NowBps <= 0 {
		current = th.Dim
	}
	m.kv(b, 0, "current", tui.Rate(s.Rate.NowBps), current)
	m.kv(b, 1, "1m avg", tui.Rate(s.Rate.Avg1mBps), tui.Style{})
	m.kv(b, 2, "5m avg", tui.Rate(s.Rate.Avg5mBps), tui.Style{})
	m.kv(b, 3, "clients", strconv.Itoa(s.Clients), tui.Style{})
	m.kv(b, 4, "sessions", fmt.Sprintf("%d pqe / %d cl", s.Sessions.PQE, s.Sessions.Classical), tui.Style{})
	tui.DrawBar(b.Sub(tui.Rect{Y: 5, W: b.Width(), H: 1}), tui.Bar{
		Label: "pressure", LabelWidth: topKeyColumn - 1, Value: fmt.Sprintf("%.2f", s.Pressure), Frac: s.Pressure,
		LabelStyle: th.Dim, Fill: th.Level(s.Pressure, 0.7, 0.9), Empty: th.Dim,
	}, th.ASCII)
	if s.RestartPending {
		b.Put("RESTART PENDING", 0, 6, th.Warn)
	}
	// The idle hint can be a whole sentence; wrap it onto the rows left.
	if s.State == "idle" && s.IdleHint != nil && *s.IdleHint != "" {
		for i, ln := range wrapText(*s.IdleHint, b.Width(), 2, th.ASCII) {
			b.Put(ln, 0, 7+i, th.Warn)
		}
	}
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
	if r.Goroutines > 0 {
		m.kvAt(b, col, y, "gor", strconv.Itoa(r.Goroutines), tui.Style{})
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
		in.Put(" "+tui.Rate(m.snap.Rate.NowBps), x, 0, tui.Style{})
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
	"+  -             refresh faster or slower",
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
