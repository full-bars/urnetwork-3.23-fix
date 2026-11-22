package urnettools

import (
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// The peaks the Now panel keeps: the highest rate seen since top started, for
// billable and for total traffic, as the live reading and as the provider's one
// and five minute averages. They are held in memory only and reset with the
// process, so they never claim to be a lifetime figure.

// topPeakRates is one stream's highest rates.
type topPeakRates struct {
	live float64
	min1 float64
	min5 float64
	has1 bool // the one minute average has had a full window
	has5 bool // the five minute average has had a full window
}

// topPeaks holds both streams and the time top started watching them.
type topPeaks struct {
	since    time.Time
	billable topPeakRates
	total    topPeakRates
}

func (p *topPeaks) reset() { *p = topPeaks{} }

// watching reports whether top has read a live rate at all. The block is left
// out of a panel that has never seen one, rather than drawn as three rows of
// dashes.
func (p *topPeaks) watching() bool { return p.billable.live > 0 || p.total.live > 0 }

// observeLive folds one live reading in. The first reading starts the clock the
// averages wait on.
func (p *topPeaks) observeLive(now time.Time, billable, total float64) {
	if p.since.IsZero() {
		p.since = now
	}
	if billable > p.billable.live {
		p.billable.live = billable
	}
	if total > p.total.live {
		p.total.live = total
	}
}

// observeAverages folds the provider's own one and five minute averages in. A
// peak counts only once top has been running long enough for that window to be
// full, so a partial window early after start cannot set it.
func (p *topPeaks) observeAverages(now time.Time, billable1m, billable5m, total1m, total5m float64) {
	if p.since.IsZero() {
		p.since = now
	}
	elapsed := now.Sub(p.since)
	if elapsed >= time.Minute {
		p.billable.has1 = true
		p.total.has1 = true
		if billable1m > p.billable.min1 {
			p.billable.min1 = billable1m
		}
		if total1m > p.total.min1 {
			p.total.min1 = total1m
		}
	}
	if elapsed >= 5*time.Minute {
		p.billable.has5 = true
		p.total.has5 = true
		if billable5m > p.billable.min5 {
			p.billable.min5 = billable5m
		}
		if total5m > p.total.min5 {
			p.total.min5 = total5m
		}
	}
}

// topPeakRows is how many rows the peaks block adds to the Now panel.
const topPeakRows = 3

// peakCell is a rate without its per second suffix: the block header says the
// numbers are rates, and the panel is narrow. A missing or empty rate is a dash.
func peakCell(v float64, set bool) string {
	if !set || v <= 0 {
		return "-"
	}
	return strings.TrimSuffix(tui.RateShort(v), "/s")
}

// drawPeaks draws the block under the traffic rows: a header, then billable and
// total, each with the live, one minute and five minute peak.
func (m *topModel) drawPeaks(b *tui.Buffer, y int) {
	th := m.theme
	p := m.peaks
	label := 8
	cell := max(4, (b.Width()-label)/3)
	right := func(s string, i, row int, style tui.Style) {
		if x := label + i*cell; x < b.Width() {
			s = tui.Truncate(s, min(cell, b.Width()-x), th.ASCII)
			if pad := cell - tui.StringWidth(s); pad > 0 {
				x += pad
			}
			b.Put(s, x, row, style)
		}
	}
	b.Put("peak/s", 0, y, th.Dim)
	for i, name := range [3]string{"live", "1m", "5m"} {
		right(name, i, y, th.Dim)
	}
	traffic := m.hasTraffic()
	liveStyle := func(v float64) tui.Style {
		if m.conn == topDisconnected || v <= 0 {
			return th.Dim
		}
		return th.OK
	}
	for i, row := range []struct {
		label string
		r     topPeakRates
	}{{"billable", p.billable}, {"total", p.total}} {
		row := row
		at := y + 1 + i
		b.Put(row.label, 0, at, th.Dim)
		right(peakCell(row.r.live, true), 0, at, liveStyle(row.r.live))
		right(peakCell(row.r.min1, traffic && row.r.has1), 1, at, tui.Style{})
		right(peakCell(row.r.min5, traffic && row.r.has5), 2, at, tui.Style{})
	}
}
