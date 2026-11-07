package urnettools

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// top_extender.go is the Extender box of `urnet-tools top`: whether the role is
// on, which ports are up, what each address family is doing, and whom it is
// helping right now and over time. It reads the same control-socket reading as
// `status`, fetched off the UI loop like the traffic counters, and is left out
// for a provider that cannot report it, so an older build shows no empty box.

// topExtenderSource is the optional source of the extender reading.
type topExtenderSource interface {
	FetchExtender(p Provider) ExtenderReading
}

func (socketTopSource) FetchExtender(p Provider) ExtenderReading {
	return ReadExtenderStatus(p)
}

// topExtenderRow is one line of the box.
type topExtenderRow struct {
	key   string
	value string
	level topLevel
}

// extenderBoxRows is the content of the box, with a level per row so a blocked
// port reads bad and a working family reads good. Empty when the provider could
// not report the extender.
func extenderBoxRows(r ExtenderReading, now time.Time) []topExtenderRow {
	if r.Source != SourceControlSocket {
		return nil
	}
	if !r.Running {
		rows := []topExtenderRow{{"state", "OFF", topWarn}}
		rows = append(rows, topExtenderRow{"turn on", "urnet-tools extender on", topInfo})
		if r.Served.Lifetime > 0 {
			rows = append(rows, topExtenderRow{"served", extenderServedText(r.Served), topInfo})
		}
		return rows
	}

	state, level := strings.ReplaceAll(r.RoleState, "_", " "), topGood
	switch r.RoleState {
	case "tcp_unavailable":
		state, level = "tcp 443 blocked: NOT listed", topBad
	case "unavailable", "error":
		level = topBad
	case "starting":
		level = topInfo
	}
	rows := []topExtenderRow{{"state", state, level}}

	var ports []string
	portLevel := topGood
	for _, port := range r.Ports {
		short := strings.Fields(port.Name)
		name := port.Name
		if len(short) > 0 {
			name = short[0] // tcp, udp, dns: the ports are in the title row
		}
		if port.Up {
			ports = append(ports, name+" bound")
			continue
		}
		ports = append(ports, name+" DOWN")
		if strings.HasPrefix(port.Name, "tcp") {
			portLevel = topBad
		} else if portLevel == topGood {
			portLevel = topWarn
		}
	}
	rows = append(rows, topExtenderRow{"ports", strings.Join(ports, " "), portLevel})

	for _, f := range r.Families {
		key := strings.ToLower(extenderFamilyName(f.IpVersion))
		switch f.State {
		case "activated":
			value := "ACTIVE"
			if f.Ip != "" {
				value += " " + f.Ip
			}
			rows = append(rows, topExtenderRow{key, value, topGood})
		case "held":
			rows = append(rows, topExtenderRow{key, "held: needs tcp 443", topBad})
		case "waiting":
			rows = append(rows, topExtenderRow{key, "waiting" + extenderNextTry(f, now), topInfo})
		default:
			value := "not active"
			if f.Detail != "" {
				value += ": " + f.Detail
			}
			rows = append(rows, topExtenderRow{key, value + extenderNextTry(f, now), topWarn})
		}
		for _, port := range f.Ports {
			level := topInfo
			if strings.Contains(port.State, "left out") {
				level = topWarn
			}
			rows = append(rows, topExtenderRow{key, port.Name + ": " + port.State, level})
		}
	}
	rows = append(rows, topExtenderRow{"clients", fmt.Sprintf("%d now", r.LiveClients), topInfo})
	rows = append(rows, topExtenderRow{"served", extenderServedText(r.Served), topInfo})
	return rows
}

func extenderServedText(s ExtenderServed) string {
	return fmt.Sprintf("%d 24h %d 7d %d all", s.Last24h, s.Last7d, s.Lifetime)
}

func (m *topModel) hasExtender() bool {
	return m.extender.Source == SourceControlSocket
}

// extenderRows is the box height: every row plus the frame.
func (m *topModel) extenderRows() int {
	return len(extenderBoxRows(m.extender, m.now())) + 2
}

// wantExtender reports whether an extender reading should be fetched now and,
// if so, marks one in flight. It reads at the snapshot cadence at most: the
// state changes on the order of minutes, so asking faster gains nothing.
func (m *topModel) wantExtender(now time.Time) (gen int, p Provider, ok bool) {
	if m.extenderBusy || len(m.providers) == 0 {
		return 0, Provider{}, false
	}
	if !m.lastExtender.IsZero() && now.Sub(m.lastExtender) < max(m.interval, topSnapshotEvery) {
		return 0, Provider{}, false
	}
	m.extenderBusy = true
	m.lastExtender = now
	return m.gen, m.provider(), true
}

// applyExtender stores one reading. A reading fetched for an earlier provider
// selection is dropped.
func (m *topModel) applyExtender(gen int, reading ExtenderReading) {
	if gen != m.gen {
		return
	}
	m.extenderBusy = false
	m.extender = reading
}

func (m *topModel) drawExtender(b *tui.Buffer) {
	th := m.theme
	rows := extenderBoxRows(m.extender, m.now())
	if len(rows) == 0 {
		b.Put("no data", 0, 0, th.Dim)
		return
	}
	for y, row := range rows {
		if y >= b.Height() {
			return
		}
		m.kvAt(b, 8, y, row.key, row.value, m.style(row.level))
	}
}
