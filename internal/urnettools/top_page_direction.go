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
// The curves are green only for now. The renderer takes a list of series per
// half, so the pink packet series slots in beside each curve through that list
// once the provider reports packet counters, with no other change here.

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
// The provider samples every lane once a second beside its total rate, so a
// lane history has the same shape and time base as TotalHistoryBps. ok is
// false for a provider that predates the block.
func (m *topModel) directionData(billable bool) (up, down []float64, newest time.Time, interval time.Duration, ok bool) {
	if !m.directionAvailable() {
		return nil, nil, time.Time{}, 0, false
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
	return up, down, m.now().Truncate(interval), interval, true
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
	up, down, newest, interval, ok := m.directionData(billable)
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
	green, _ := remotePalette(th)
	upGroup, downGroup := directionGroups(id)
	chart := remoteChart{
		Up:       []remoteSeries{{Label: "upload", Style: green, Group: upGroup, Values: up}},
		Down:     []remoteSeries{{Label: "download", Style: green, Group: downGroup, Values: down}},
		Now:      m.now(),
		Newest:   newest,
		Interval: interval,
		Window:   remoteDefaultWindow,
		Scales:   m.directionScales,
		ASCII:    th.ASCII,
		Dim:      th.Dim,
		Text:     th.Accent,
	}
	drawRemoteChart(box, chart)
}
