package urnettools

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect/internal/tui"
)

// top_overview.go is the screen a host with several providers needs: one row
// per provider with the figures an operator compares before tabbing into one.
// The rows are a pure function of the per provider snapshots top already
// fetches, so the screen can be tested without a terminal or a socket.

// topOverviewRow is one provider row of the overview screen.
type topOverviewRow struct {
	index        int // the provider index the row stands for
	name         string
	version      string
	state        string
	level        topLevel
	billable     string
	clients      int
	proxiesUp    int
	proxiesTotal int
	extender     string
	spark        string
}

// buildOverviewRows is the pure function behind the screen: one row per
// provider, in order, from the snapshots and extender readings top already
// holds. A provider with no snapshot yet says so and leaves the figures empty.
func buildOverviewRows(providers []Provider, snaps []*NodeSnapshot, exts []ExtenderReading, th tui.Theme) []topOverviewRow {
	rows := make([]topOverviewRow, 0, len(providers))
	for i, p := range providers {
		row := topOverviewRow{index: i, name: topNodeLabel(p), state: "no data", level: topInfo}
		if i < len(exts) {
			row.extender = overviewExtender(exts[i])
		} else {
			row.extender = "—"
		}
		var s *NodeSnapshot
		if i < len(snaps) {
			s = snaps[i]
		}
		if s == nil {
			rows = append(rows, row)
			continue
		}
		row.version = orDash(s.Version)
		row.state = strings.ToUpper(s.State)
		row.level = stateLevel(s.State)
		row.billable = tui.Rate(s.Rate.NowBps)
		row.clients = s.Clients
		row.proxiesUp = s.Proxies.Up
		row.proxiesTotal = s.Proxies.Up + s.Proxies.Degraded + s.Proxies.Connecting + s.Proxies.Dead
		row.spark = overviewSpark(s.Rate.HistoryBps, th.ASCII)
		rows = append(rows, row)
	}
	return rows
}

// stateLevel maps the provider's state word onto the severity role that
// speaks it, so the state dot carries the same meaning as the Events icons.
func stateLevel(state string) topLevel {
	switch state {
	case "flowing":
		return topGood
	case "idle":
		return topWarn
	case "degraded":
		return topBad
	}
	return topInfo
}

// overviewExtender is the extender column: serving, off, or a dash when the
// provider could not report it.
func overviewExtender(r ExtenderReading) string {
	if r.Source != SourceControlSocket {
		return "—"
	}
	if r.Running {
		return "serving"
	}
	return "off"
}

// overviewSpark draws the provider's own rate history as one sparkline, the
// same glyph style the rest of top uses.
func overviewSpark(h []float64, ascii bool) string {
	if len(h) == 0 {
		return ""
	}
	n := min(24, len(h))
	vals := make([]uint64, n)
	for i, v := range h[len(h)-n:] {
		vals[i] = uint64(max(v, 0))
	}
	return tui.Spark(vals, 24, ascii)
}

// drawOverview draws the overview screen and returns the provider index of the
// selected row, or -1 when no row is selected. The rows are rebuilt from the
// model's per provider snapshots every frame, so the screen is a pure function
// of the model alone.
func (m *topModel) drawOverview(b *tui.Buffer) int {
	th := m.theme
	rows := buildOverviewRows(m.providers, m.providerSnaps, m.providerExt, th)
	in := tui.DrawBox(b, "Providers", th.Frame, th.Border, th.Accent, th.ASCII)
	if in.Height() < 2 {
		return -1
	}
	header := "name       version      state        billable   clients proxies extender spark"
	in.Put(tui.Truncate(header, in.Width(), th.ASCII), 0, 0, th.Dim)
	for i, r := range rows {
		y := i + 1
		if y >= in.Height() {
			break
		}
		st := tui.Style{}
		if i == m.overviewSel {
			st = th.Accent
		}
		x := in.Put(tui.Truncate(fmt.Sprintf("%2d %s", r.index+1, r.name), 12, th.ASCII), 0, y, st)
		x = in.Put(tui.Truncate(r.version, 12, th.ASCII), x+1, y, st)
		x = in.Put(tui.Truncate(string(eventIcon(r.level, th))+" "+r.state, 12, th.ASCII), x+1, y, m.style(r.level))
		x = in.Put(tui.Truncate(r.billable, 10, th.ASCII), x+1, y, st)
		x = in.Put(fmt.Sprintf("%6d", r.clients), x+1, y, st)
		x = in.Put(tui.Truncate(fmt.Sprintf("%d/%d", r.proxiesUp, r.proxiesTotal), 7, th.ASCII), x+1, y, st)
		x = in.Put(tui.Truncate(r.extender, 8, th.ASCII), x+1, y, st)
		in.Put(r.spark, x+1, y, th.Graph)
	}
	if m.overviewSel < 0 || m.overviewSel >= len(rows) {
		return -1
	}
	return rows[m.overviewSel].index
}

// overviewRowAt maps a screen row onto the provider index it stands for, -1
// when the row is the header, the frame or past the last provider. It is the
// hit test a future click or arrow key uses.
func overviewRowAt(rows []topOverviewRow, y int) int {
	if y < 1 || y-1 >= len(rows) {
		return -1
	}
	return rows[y-1].index
}
