package urnettools

import (
	"fmt"
	"sort"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// topCarrierRow is one carrier of the Extender box.
type topCarrierRow struct {
	name    string
	served  uint64
	refused uint64
	reasons []string
	ipv4    bool
	ipv6    bool
}

// topCarrierRate is the per-minute ring for one carrier. The extender poll
// hands back cumulative served counts, so the ring is the per-minute delta,
// not the raw counter: a sparkline of the raw counter climbs forever.
const topCarrierRing = 24

type topCarrierRate struct {
	base          time.Time
	servedAtStart uint64
	ring          []uint64
}

type topCarrierHistory struct {
	per map[string]*topCarrierRate
}

func (c *topCarrierHistory) reset() { *c = topCarrierHistory{} }

func (c *topCarrierHistory) push(now time.Time, carrier string, served uint64) {
	if c.per == nil {
		c.per = make(map[string]*topCarrierRate)
	}
	e := c.per[carrier]
	if e == nil {
		c.per[carrier] = &topCarrierRate{base: now, servedAtStart: served}
		return
	}
	if e.base.IsZero() {
		e.base = now
		e.servedAtStart = served
		return
	}
	if served < e.servedAtStart {
		// The counter dropped: a new proxy was adopted or the provider
		// restarted. Fold the new value as the baseline and do not invent a
		// rate.
		e.base, e.servedAtStart = now, served
		return
	}
	if now.Sub(e.base) < time.Minute {
		return
	}
	rate := float64(served-e.servedAtStart) * float64(time.Minute) / float64(now.Sub(e.base))
	e.ring = append(e.ring, uint64(rate))
	if len(e.ring) > topCarrierRing {
		e.ring = e.ring[len(e.ring)-topCarrierRing:]
	}
	e.base, e.servedAtStart = now, served
}

// carrierRows turns one extender reading into the carrier rows. The reading is
// nil-safe. A missing field stays missing, never zero.
func carrierRows(r ExtenderReading) []topCarrierRow {
	if r.Source != SourceControlSocket || !r.Known {
		return nil
	}
	activatedV4, activatedV6 := false, false
	for _, f := range r.Families {
		switch f.IpVersion {
		case 4:
			if f.State == "activated" {
				activatedV4 = true
			}
		case 6:
			if f.State == "activated" {
				activatedV6 = true
			}
		}
	}
	rows := make([]topCarrierRow, 0, len(r.Carriers)+1)
	for _, c := range r.Carriers {
		rows = append(rows, topCarrierRow{name: c.Carrier, served: c.Served, refused: c.Refused, reasons: topRefusalReasons(c.RefusalsByStage), ipv4: activatedV4, ipv6: activatedV6})
	}
	return rows
}

func topRefusalReasons(m map[string]uint64) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(out)
	return out
}

func (m *topModel) drawCarriers(b *tui.Buffer) {
	if m.extender.Source != SourceControlSocket || !m.extender.Known {
		if b.Height() > 0 {
			b.Put("no data", 0, 0, m.theme.Dim)
		}
		return
	}
	rows := carrierRows(m.extender)
	if len(rows) == 0 {
		if b.Height() > 0 {
			b.Put("no data", 0, 0, m.theme.Dim)
		}
		return
	}
	y := 0
	for _, row := range rows {
		if y >= b.Height() {
			return
		}
		dot := func(ok bool) string {
			if ok {
				return string(rune(0x25cf)) // filled circle: green
			}
			return string(rune(0x25cb)) // open circle: red/disabled
		}
		spark := ""
		if r := m.carriers.per[row.name]; r != nil && len(r.ring) > 1 {
			spark = " " + tui.Spark(r.ring, 8, m.theme.ASCII)
		}
		line := fmt.Sprintf("%-12s %8d %8d %s%s %s", truncate(row.name, 12), row.served, row.refused, dot(row.ipv4), dot(row.ipv6), spark)
		b.Put(line, 0, y, m.theme.Dim)
		y++
		for _, r := range row.reasons {
			if y >= b.Height() {
				return
			}
			b.Put("    refused "+r, 0, y, m.theme.Dim)
			y++
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
