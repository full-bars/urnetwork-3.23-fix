package urnettools

import (
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The Remote panel is the mirrored transfer chart, wired to the same provider
// history the throughput graphs draw. Billable traffic goes above the center
// line and total traffic below it, which is what the provider reports today:
// there is no separate upload and download counter and no packet counter, so
// the pink packet curve and the two halves per direction are left out rather
// than filled with made-up data. The renderer already takes a list of series
// per half, so those slot in unchanged once the provider reports them.
//
// The panel shares the graph band instead of taking a band of its own, so the
// layout of the rest of the screen is untouched. Key r shows and hides it.

const (
	// topRemoteRows is the box height the Remote panel asks for: a frame plus
	// eleven rows, which is enough for the legends, both plots and both peak
	// labels with a few rows of curve each.
	topRemoteRows = 13
	// topRemoteMinBand is the shortest graph band that holds the Remote panel
	// and still leaves the two throughput graphs their own minimum, so turning
	// the panel on never costs a graph.
	topRemoteMinBand = topRemoteRows + topTwoGraphsMinRows
)

// remoteSplit takes the Remote panel off the bottom of the graph band. With the
// panel hidden, or a band too short for both, the graphs keep the whole band.
func (m *topModel) remoteSplit(r tui.Rect) (graphs, remote tui.Rect, ok bool) {
	if !m.remote || r.H < topRemoteMinBand {
		return r, tui.Rect{}, false
	}
	rows := tui.SplitRows(r, tui.Flex(1), tui.Fixed(topRemoteRows))
	return rows[0], rows[1], true
}

// remotePalette is the pair of curve colors: bytes in the client app's green
// and packets in its pink, or the theme's own two roles where the theme carries
// no fixed palette. The pink is one value for every packet curve, so a theme
// lane can swap it in one place.
func remotePalette(th tui.Theme) (bytes, packets tui.Style) {
	if th.ASCII {
		return tui.Style{}, tui.Style{}
	}
	if th.Name == "colorblind" {
		return th.OK, th.Warn
	}
	if th.Name == "neon" {
		return tui.Style{FG: th.SeriesBytesFG}, tui.Style{FG: th.SeriesPacketsFG}
	}
	return tui.Style{FG: tui.Xterm(120)}, tui.Style{FG: tui.Xterm(213)}
}

// remoteData is what the panel draws this frame: the two halves, the time of
// the newest sample and the spacing between samples.
func (m *topModel) remoteData() (up, down []float64, newest time.Time, interval time.Duration, ok bool) {
	s := m.snap
	if s == nil {
		return nil, nil, time.Time{}, 0, false
	}
	interval = time.Second
	if s.Rate.HistoryIntervalSeconds > 0 {
		interval = time.Duration(s.Rate.HistoryIntervalSeconds * float64(time.Second))
	}
	// The client-side ring carries one slot per second and marks missed
	// seconds as gaps; the provider's own ring is the fallback before the
	// first traffic poll lands.
	if len(m.trafficHistory.billable) > 0 {
		down = m.trafficHistory.total
		if s.Traffic == nil {
			down = nil
		}
		return m.trafficHistory.billable, down, m.trafficNewest(time.Second), time.Second, true
	}
	up = s.Rate.HistoryBps
	if len(up) == 0 {
		return nil, nil, time.Time{}, 0, false
	}
	if s.Traffic != nil {
		down = s.Traffic.TotalHistoryBps
	}
	return up, down, m.snapNewest(interval), interval, true
}

// trafficNewest is the time of the newest reading in the client ring: when it
// arrived. Before any reading it is the clock at draw time, to the second.
func (m *topModel) trafficNewest(interval time.Duration) time.Time {
	if a := m.trafficHistory.arrived; !a.IsZero() {
		return a
	}
	return m.now().Truncate(interval)
}

// snapNewest is the time of the newest slot of the provider's own ring: when
// the snapshot that carried it arrived.
func (m *topModel) snapNewest(interval time.Duration) time.Time {
	if !m.lastAnswer.IsZero() {
		return m.lastAnswer
	}
	return m.now().Truncate(interval)
}

// remoteDirData is the client app's form of the chart: billable upload and
// download, in bytes and in packets. It comes from the live 1 s ring once that
// carries packet readings, and from the snapshot's direction and packet blocks
// before the first traffic poll. ok is false for a provider without the packet
// block, which keeps the billable over total form.
func (m *topModel) remoteDirData() (upB, downB, upP, downP []float64, newest time.Time, interval time.Duration, ok bool) {
	s := m.snap
	if s == nil {
		return nil, nil, nil, nil, time.Time{}, 0, false
	}
	if h := &m.trafficHistory; len(h.billable) > 0 {
		if !h.hasPackets() {
			return nil, nil, nil, nil, time.Time{}, 0, false
		}
		return h.upBytes, h.downBytes, h.upPackets, h.downPackets, m.trafficNewest(time.Second), time.Second, true
	}
	if s.Traffic == nil || s.Traffic.Directions == nil || s.Traffic.Packets == nil {
		return nil, nil, nil, nil, time.Time{}, 0, false
	}
	d, p := s.Traffic.Directions, s.Traffic.Packets
	upB, downB, upP, downP = d.BillableTx.HistoryBps, d.BillableRx.HistoryBps, p.BillableTx.HistoryPps, p.BillableRx.HistoryPps
	if len(upB) == 0 || len(downB) == 0 || len(upP) == 0 || len(downP) == 0 {
		return nil, nil, nil, nil, time.Time{}, 0, false
	}
	interval = time.Second
	if s.Rate.HistoryIntervalSeconds > 0 {
		interval = time.Duration(s.Rate.HistoryIntervalSeconds * float64(time.Second))
	}
	return upB, downB, upP, downP, m.snapNewest(interval), interval, true
}

// snapshotDirRates is the direction and packet reading of the snapshot that
// lines up with entry i of a history n long, newest entries aligned, used to
// seed the live ring. Unknown values are NaN.
func snapshotDirRates(s *NodeSnapshot, i, n int) topDirRates {
	out := topNoDirRates()
	if s == nil || s.Traffic == nil || s.Traffic.Directions == nil || s.Traffic.Packets == nil {
		return out
	}
	d, p := s.Traffic.Directions, s.Traffic.Packets
	for k, h := range [][]float64{d.BillableTx.HistoryBps, d.BillableRx.HistoryBps, p.BillableTx.HistoryPps, p.BillableRx.HistoryPps} {
		if j := len(h) - (n - i); j >= 0 && j < len(h) {
			out[k] = h[j]
		}
	}
	return out
}

// drawRemote draws the Remote panel from the model alone.
func (m *topModel) drawRemote(b *tui.Buffer) {
	th := m.theme
	if m.remoteScales == nil {
		m.remoteScales = map[string]*remoteScale{}
	}
	if upB, downB, upP, downP, newest, interval, ok := m.remoteDirData(); ok {
		bytesStyle, packetStyle := remotePalette(th)
		drawRemoteChart(b, remoteChart{
			Up: []remoteSeries{
				{Label: "upload", Style: bytesStyle, Group: "remote upload", Values: upB},
				{Style: packetStyle, Group: "remote upload packets", Glyph: '+', Packets: true, Values: upP},
			},
			Down: []remoteSeries{
				{Label: "download", Style: bytesStyle, Group: "remote download", Values: downB},
				{Style: packetStyle, Group: "remote download packets", Glyph: '+', Packets: true, Values: downP},
			},
			Now:      m.now(),
			Newest:   newest,
			Interval: interval,
			Window:   m.remoteWindow(),
			Scales:   m.remoteScales,
			// One byte scale and one packet scale for both halves, as in
			// the client app, so a small upload looks small next to a big
			// download.
			Shared: true,
			ASCII:  th.ASCII,
			Dim:    th.Dim,
			Text:   th.Accent,
		})
		return
	}
	up, down, newest, interval, ok := m.remoteData()
	if !ok {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		m.drawWatermark(b, 1)
		b.Put(tui.Truncate(msg, b.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	bytesStyle, _ := remotePalette(th)
	chart := remoteChart{
		Up:       []remoteSeries{{Label: "billable", Style: bytesStyle, Group: "billable", Values: up}},
		Now:      m.now(),
		Newest:   newest,
		Interval: interval,
		Window:   m.remoteWindow(),
		Scales:   m.remoteScales,
		ASCII:    th.ASCII,
		Dim:      th.Dim,
		Text:     th.Accent,
	}
	if len(down) > 0 {
		// Its own scale group: total traffic is billable plus everything else,
		// so one shared height would flatten the billable curve to nothing.
		chart.Down = []remoteSeries{{Label: "total", Style: bytesStyle, Group: "total", Values: down}}
	}
	drawRemoteChart(b, chart)
}
