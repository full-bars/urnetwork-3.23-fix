package urnettools

import (
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The IP family page draws billable traffic split by IP version. upload is tx,
// the bytes the provider sent away from the client, above the centre line, and
// download is rx below it, as on the direction pages. Each half carries two
// series: IPv4 in the green the other pages use, IPv6 in the URnetwork blue. The
// provider counts IPv6 and derives IPv4 by subtraction, so both series are real
// figures from the same second.
//
// A one row bar under the title states the window share of the two families.
// The page is offered only when the provider sends the family block, so an
// older provider keeps the pages it always had.

// familyPageID is the page's id and hit-region name.
const familyPageID = "family"

// familyShareBar is the row the share bar takes under the box title.
const familyShareBar = 1

// familyAvailable reports whether the provider sends the IP family split inside
// the direction block. A provider that predates it leaves Families nil, which
// keeps the page out of the strip.
func (m *topModel) familyAvailable() bool {
	return m.snap != nil && m.snap.Traffic != nil && m.snap.Traffic.Directions != nil &&
		m.snap.Traffic.Directions.Families != nil
}

// familyPalette is the pair of curve colours: the byte green of the other pages
// for IPv4, and the URnetwork blue for IPv6. A theme with neither colour nor
// attributes leaves both styles empty and the ASCII glyphs tell the series
// apart. The colourblind theme uses its accent for IPv6 so the pair stays
// distinguishable.
func familyPalette(th tui.Theme) (v4, v6 tui.Style) {
	v4, _ = remotePalette(th)
	if th.ASCII {
		return v4, tui.Style{}
	}
	if th.Name == "colorblind" {
		return v4, th.Accent
	}
	return v4, tui.Style{FG: tui.RGB(0x00, 0x99, 0xFF)}
}

// familyData is what the page draws: the IPv4 and IPv6 billable histories for
// upload and download, the time of the newest sample and its spacing. ok is
// false for a provider that predates the family block.
func (m *topModel) familyData() (v4Up, v6Up, v4Down, v6Down []float64, newest time.Time, interval time.Duration, ok bool) {
	if !m.familyAvailable() {
		return nil, nil, nil, nil, time.Time{}, 0, false
	}
	interval = time.Second
	if m.snap.Rate.HistoryIntervalSeconds > 0 {
		interval = time.Duration(m.snap.Rate.HistoryIntervalSeconds * float64(time.Second))
	}
	f := m.snap.Traffic.Directions.Families
	// Date the samples from the snapshot that carried them: while that snapshot
	// is retained its history ages out of the window, and the next snapshot
	// dates its samples afresh. A model handed a snapshot directly (the demo and
	// the golden fixtures) has no acceptance time and keeps the clock.
	newest = m.snapAt
	if newest.IsZero() {
		newest = m.now()
	}
	return f.BillableTx.V4.HistoryBps, f.BillableTx.V6.HistoryBps,
		f.BillableRx.V4.HistoryBps, f.BillableRx.V6.HistoryBps,
		newest.Truncate(interval), interval, true
}

// familyShare is the whole-percentage split of two byte figures over the window.
// idle reports a window with no traffic at all, which has no share to state.
// The two percentages always sum to 100: IPv6 takes the remainder, so rounding
// cannot leave the pair at 99 or 101.
func familyShare(v4, v6 float64) (v4pct, v6pct int, idle bool) {
	total := v4 + v6
	if !(total > 0) {
		return 0, 0, true
	}
	pct := int(math.Round(v4 / total * 100))
	pct = min(max(pct, 0), 100)
	return pct, 100 - pct, false
}

// sumSeries adds the samples of a history, ignoring anything that is not a
// positive figure. The histories are per-second rates, so the sum is the bytes
// the window carried.
func sumSeries(vals []float64) float64 {
	var sum float64
	for _, v := range vals {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			sum += v
		}
	}
	return sum
}

// familyShareText is the share bar's line: the two percentages, or "idle" with
// zeroes for a window that carried nothing.
func familyShareText(v4up, v6up, v4down, v6down []float64) string {
	return familyShareTextWindow(v4up, v6up, v4down, v6down, time.Time{}, time.Time{}, 0, 0)
}

// sumSeriesWindow adds the samples the chart still shows: those no older than
// the window before now.
func sumSeriesWindow(vals []float64, newest, now time.Time, interval, window time.Duration) float64 {
	if window <= 0 || interval <= 0 || newest.IsZero() || now.IsZero() {
		return sumSeries(vals)
	}
	start := now.Add(-window)
	var sum float64
	for i := len(vals) - 1; i >= 0; i-- {
		at := newest.Add(-time.Duration(len(vals)-1-i) * interval)
		if at.Before(start) {
			break
		}
		if v := vals[i]; v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			sum += v
		}
	}
	return sum
}

// familyShareTextWindow states the split of the samples the chart actually
// shows, rather than the entire history the provider has accumulated.
func familyShareTextWindow(v4up, v6up, v4down, v6down []float64, newest, now time.Time, interval, window time.Duration) string {
	v4pct, v6pct, idle := familyShare(
		sumSeriesWindow(v4up, newest, now, interval, window)+sumSeriesWindow(v4down, newest, now, interval, window),
		sumSeriesWindow(v6up, newest, now, interval, window)+sumSeriesWindow(v6down, newest, now, interval, window))
	line := fmt.Sprintf("IPv4 %d%%  IPv6 %d%%", v4pct, v6pct)
	if idle {
		line += "  idle"
	}
	return line
}

// familyPageAvailable offers the IP family page when the provider reports the
// family block.
func (m *topModel) familyPageAvailable(tui.Rect) bool { return m.familyAvailable() }

// drawFamilyPage paints the IP family page.
func (m *topModel) drawFamilyPage(b *tui.Buffer, content tui.Rect) {
	th := m.theme
	box := tui.DrawBox(b.Sub(content), "IPv4 / IPv6", th.Frame, th.Border, th.Accent, th.ASCII)
	m.record("box:"+familyPageID, content)
	v4Up, v6Up, v4Down, v6Down, newest, interval, ok := m.familyData()
	if !ok {
		msg := "waiting for the provider"
		if m.conn == topDisconnected {
			msg = "no data: " + m.lastErr
		}
		box.Put(tui.Truncate(msg, box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	if len(v4Up) == 0 && len(v6Up) == 0 && len(v4Down) == 0 && len(v6Down) == 0 {
		// A new provider sends the block before its first sample.
		box.Put(tui.Truncate("waiting for the first IP family samples", box.Width(), th.ASCII), 0, 0, th.Dim)
		return
	}
	m.drawFamilyShare(box, v4Up, v6Up, v4Down, v6Down, newest, m.now(), interval, remoteDefaultWindow)
	if box.Height() <= familyShareBar {
		return
	}
	// The share bar keeps the first row; the chart draws in the rest of the box.
	inner := box.Sub(tui.Rect{X: 0, Y: familyShareBar, W: box.Width(), H: box.Height() - familyShareBar})
	if inner.Height() <= 0 {
		return
	}
	if m.familyScales == nil {
		m.familyScales = map[string]*remoteScale{}
	}
	v4Style, v6Style := familyPalette(th)
	upGroup, downGroup := directionGroups(familyPageID)
	chart := remoteChart{
		Up: []remoteSeries{
			{Label: "IPv4", Style: v4Style, Glyph: '#', OverlapGlyph: '*', Group: upGroup, Values: v4Up},
			{Label: "IPv6", Style: v6Style, Glyph: '+', OverlapGlyph: '*', Group: upGroup, Values: v6Up},
		},
		Down: []remoteSeries{
			{Label: "IPv4", Style: v4Style, Glyph: '#', OverlapGlyph: '*', Group: downGroup, Values: v4Down},
			{Label: "IPv6", Style: v6Style, Glyph: '+', OverlapGlyph: '*', Group: downGroup, Values: v6Down},
		},
		Now:      m.now(),
		Newest:   newest,
		Interval: interval,
		Window:   remoteDefaultWindow,
		Scales:   m.familyScales,
		ASCII:    th.ASCII,
		Dim:      th.Dim,
		Text:     th.Accent,
	}
	drawRemoteChart(inner, chart)
}

// drawFamilyShare paints the window share bar in the box's first row, each
// figure in its series colour so it reads against the curves below.
func (m *topModel) drawFamilyShare(box *tui.Buffer, v4up, v6up, v4down, v6down []float64, newest, now time.Time, interval, window time.Duration) {
	th := m.theme
	v4Style, v6Style := familyPalette(th)
	v4pct, v6pct, idle := familyShare(
		sumSeriesWindow(v4up, newest, now, interval, window)+sumSeriesWindow(v4down, newest, now, interval, window),
		sumSeriesWindow(v6up, newest, now, interval, window)+sumSeriesWindow(v6down, newest, now, interval, window))
	row := 0
	x := box.Put(tui.Truncate("IPv4", box.Width(), th.ASCII), 0, row, v4Style)
	x = box.Put(fmt.Sprintf(" %d%%", v4pct), x, row, v4Style)
	x += 2
	x = box.Put(tui.Truncate("IPv6", max(box.Width()-x, 0), th.ASCII), x, row, v6Style)
	x = box.Put(fmt.Sprintf(" %d%%", v6pct), x, row, v6Style)
	if idle {
		box.Put(tui.Truncate("  idle", max(box.Width()-x, 0), th.ASCII), x, row, th.Dim)
	}
}
