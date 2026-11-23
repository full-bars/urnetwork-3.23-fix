package urnettools

import (
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// Pages for billable and total traffic split by direction. upload is tx, the
// bytes the provider sent away from the client, and it is drawn above the
// centre line. download is rx, the bytes it received toward the client, drawn
// below it. Both layers count them that way: the packet path in ip.go and the
// socket path in net.go. The two pages reuse the Remote renderer, so they get
// its legends, corner averages and peak labels unchanged.
//
// The billable page adds the billable packet rate as a pink series per half,
// in a scale group of its own so each curve reaches its own peak. The renderer
// takes a list of series per half, so the packet series needs no other change
// here. Total traffic has no packet counters, so that page draws bytes only.

// directionAvailable reports whether the provider sends the per-direction
// block. A provider that predates it leaves Traffic.Directions nil, which is
// what keeps the pages out of the strip.
func (m *topModel) directionAvailable() bool {
	return m.snap != nil && m.snap.Traffic != nil && m.snap.Traffic.Directions != nil
}

// directionGroups names the two scale groups of a page. Each half eases on its
// own peak: upload is a fraction of download on most links, and one shared
// height would flatten the smaller curve to nothing.
func directionGroups(page string) (up, down string) {
	return page + " upload", page + " download"
}

// directionData is what a direction page draws: the upload and download
// histories for one quantity, the time of the newest sample and its spacing.
// It also returns the packet histories (packet rates) for billable traffic;
// nil when the provider does not report them. ok is false for a provider that
// predates the block.
func (m *topModel) directionData(billable bool) (up, down []float64, pUp, pDown []float64, newest time.Time, interval time.Duration, ok bool) {
	if !m.directionAvailable() {
		return nil, nil, nil, nil, time.Time{}, 0, false
	}
	interval = time.Second
	if m.snap.Rate.HistoryIntervalSeconds > 0 {
		interval = time.Duration(m.snap.Rate.HistoryIntervalSeconds * float64(time.Second))
	}
	d := m.snap.Traffic.Directions
	if billable {
		up, down = d.BillableTx.HistoryBps, d.BillableRx.HistoryBps
	} else {
		up, down = d.TotalTx.HistoryBps, d.TotalRx.HistoryBps
	}
	// Pull the billable packet histories from the provider's Traffic.Packets
	// block. Packets exist only for billable traffic, so the total page gets
	// none and the packet series is nil there.
	if m.snap.Traffic != nil && m.snap.Traffic.Packets != nil && billable {
		pp := m.snap.Traffic.Packets
		pUp, pDown = pp.BillableTx.HistoryPps, pp.BillableRx.HistoryPps
	}
	return up, down, pUp, pDown, m.now().Truncate(interval), interval, true
}

// billablePageAvailable offers the billable direction page when the provider
// reports the block.
func (m *topModel) billablePageAvailable(tui.Rect) bool { return m.directionAvailable() }

// totalPageAvailable offers the total direction page on the same condition.
func (m *topModel) totalPageAvailable(tui.Rect) bool { return m.directionAvailable() }

// drawBillablePage paints the billable direction page.
func (m *topModel) drawBillablePage(b *tui.Buffer, content tui.Rect) {
	m.drawDirectionPage(b, content, "billable", "Billable", true)
}

// drawTotalPage paints the total direction page.
func (m *topModel) drawTotalPage(b *tui.Buffer, content tui.Rect) {
	m.drawDirectionPage(b, content, "total", "Total", false)
}

// drawDirectionPage paints one direction page inside the content rectangle.
func (m *topModel) drawDirectionPage(b *tui.Buffer, content tui.Rect, id, title string, billable bool) {
	th := m.theme
	box := tui.DrawBox(b.Sub(content), title, th.Frame, th.Border, th.Accent, th.ASCII)
	m.record("box:"+id, content)
	up, down, pUp, pDown, newest, interval, ok := m.directionData(billable)
	if !ok {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		box.Put(tui.Truncate(msg, box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	if len(up) == 0 && len(down) == 0 {
		// A new provider sends the block before its first sample. The chart's
		// own empty state names the Remote panel, so the page says its piece.
		box.Put(tui.Truncate("waiting for the first direction samples", box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	if m.directionScales == nil {
		m.directionScales = map[string]*remoteScale{}
	}
	bytesStyle, packetStyle := remotePalette(th)
	upGroup, downGroup := directionGroups(id)
	chart := remoteChart{
		Up: []remoteSeries{
			{Label: "upload", Style: bytesStyle, Group: upGroup, Values: up},
		},
		Down: []remoteSeries{
			{Label: "download", Style: bytesStyle, Group: downGroup, Values: down},
		},
		Now:      m.now(),
		Newest:   newest,
		Interval: interval,
		Window:   m.remoteWindow(),
		Scales:   m.directionScales,
		ASCII:    th.ASCII,
		Dim:      th.Dim,
		Text:     th.Accent,
	}
	// Add the pink packet series on billable pages only, and only when the
	// provider reports them (nil Packets means no pink and no packet figure).
	if billable && m.snap.Traffic != nil && m.snap.Traffic.Packets != nil {
		chart.Up = append(chart.Up, remoteSeries{
			Label:   "",
			Style:   packetStyle,
			Group:   upGroup + " packets",
			Glyph:   '+',
			Packets: true,
			Values:  pUp,
		})
		chart.Down = append(chart.Down, remoteSeries{
			Label:   "",
			Style:   packetStyle,
			Group:   downGroup + " packets",
			Glyph:   '+',
			Packets: true,
			Values:  pDown,
		})
	}
	drawRemoteChart(box, chart)
}
