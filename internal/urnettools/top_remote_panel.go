package urnettools

import (
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The Remote panel is the mirrored transfer chart, wired to the same provider
// history the throughput graphs draw. Billable traffic goes above the centre
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

// remotePalette is the pair of curve colours: the client app's green and pink,
// or the theme's own two roles where the theme carries no fixed palette.
func remotePalette(th tui.Theme) (bytes, other tui.Style) {
	if th.ASCII {
		return tui.Style{}, tui.Style{}
	}
	if th.Name == "colorblind" {
		return th.OK, th.Warn
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
		return m.trafficHistory.billable, down, m.now().Truncate(time.Second), time.Second, true
	}
	up = s.Rate.HistoryBps
	if len(up) == 0 {
		return nil, nil, time.Time{}, 0, false
	}
	if s.Traffic != nil {
		down = s.Traffic.TotalHistoryBps
	}
	return up, down, m.now().Truncate(interval), interval, true
}

// drawRemote draws the Remote panel from the model alone.
func (m *topModel) drawRemote(b *tui.Buffer) {
	th := m.theme
	up, down, newest, interval, ok := m.remoteData()
	if !ok {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		b.Put(tui.Truncate(msg, b.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	if m.remoteScales == nil {
		m.remoteScales = map[string]*remoteScale{}
	}
	bytesStyle, _ := remotePalette(th)
	chart := remoteChart{
		Up:       []remoteSeries{{Label: "billable", Style: bytesStyle, Group: "billable", Values: up}},
		Now:      m.now(),
		Newest:   newest,
		Interval: interval,
		Window:   remoteDefaultWindow,
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
