package urnettools

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// statusLevel describes a row without inferring missing readings as healthy.
type statusLevel string

const (
	statusOK      statusLevel = "ok"
	statusWarn    statusLevel = "warning"
	statusBad     statusLevel = "broken"
	statusUnknown statusLevel = "unknown"
)

type statusRow struct {
	Name  string      `json:"name"`
	Level statusLevel `json:"level"`
	Text  string      `json:"text"`
}

// makeStatusRows keeps presentation separate from socket and supervisor I/O.
// A nil snapshot, nil SIGPIPE risk, or unknown extender remains unknown.
func makeStatusRows(p Provider, s *NodeSnapshot, e ExtenderReading, sigpipeRisk *bool) []statusRow {
	version := p.Version
	if s != nil && s.Version != "" {
		version = s.Version
	}
	versionLevel := statusOK
	if version == "" {
		version, versionLevel = "unknown", statusUnknown
	}
	rows := []statusRow{{"Version", versionLevel, version}}
	health := statusRow{"Health", statusUnknown, "live status unavailable"}
	if !p.Running {
		health.Level, health.Text = statusBad, "provider stopped; run urnet-tools start"
	} else if s != nil {
		health.Level, health.Text = statusOK, s.State
		if s.State == "" {
			health.Level, health.Text = statusUnknown, "state unavailable"
		} else if s.State == "degraded" || s.State == "starting" || s.RestartPending {
			health.Level = statusWarn
			if s.RestartPending {
				health.Text += "; restart pending"
			}
		}
	}
	rows = append(rows, health)
	proxy := statusRow{"Proxies", statusUnknown, "unavailable"}
	if s != nil {
		v := s.Proxies
		proxy.Level = statusOK
		proxy.Text = fmt.Sprintf("%d up, %d degraded, %d connecting, %d dead", v.Up, v.Degraded, v.Connecting, v.Dead)
		if v.Parked != nil {
			proxy.Text += fmt.Sprintf(", %d parked", *v.Parked)
		}
		if v.TrimReported {
			proxy.Text += fmt.Sprintf(", trim %d/%d", v.TrimCap, v.Configured)
		}
		if v.Degraded > 0 || v.Dead > 0 || v.Connecting > 0 || (v.Parked != nil && *v.Parked > 0) {
			proxy.Level = statusWarn
		}
	}
	rows = append(rows, proxy)
	if s != nil && s.Proxies.UDP != nil {
		u := s.Proxies.UDP
		row := udprow(u)
		level := statusOK
		if u.NoRelay > 0 || u.Mismatch > 0 || u.Unknown > 0 || u.Unchecked > 0 {
			level = statusWarn
		}
		rows = append(rows, statusRow{"UDP", level, strings.Join(row.segs, row.sep)})
	}
	extender := statusRow{"Extender", statusUnknown, "unavailable"}
	if e.Known {
		extender.Level, extender.Text = statusOK, "off"
		if e.Running {
			extender.Text = "active"
		} else if e.Activating {
			extender.Level, extender.Text = statusWarn, "activating"
		}
	}
	rows = append(rows, extender)
	unit := statusRow{"Unit", statusUnknown, "not reported"}
	if p.Unit != "" {
		unit.Text = p.Unit
		if sigpipeRisk != nil {
			unit.Level = statusOK
			if *sigpipeRisk {
				unit.Level = statusWarn
				unit.Text += "; SIGPIPE risk; run urnet-tools update"
			}
		}
	}
	rows = append(rows, unit)
	resources := statusRow{"Resources", statusUnknown, "unavailable"}
	if s != nil {
		resources.Level = statusOK
		resources.Text = fmt.Sprintf("heap %s, %d goroutines, pressure %.2f", formatBytes(float64(s.Resources.HeapInuseBytes)), s.Resources.Goroutines, s.Pressure)
	}
	return append(rows, resources)
}

// renderStatusSummary renders the one-screen status summary for one provider:
// the rows from the summary model. plain disables color and escape sequences.
func renderStatusSummary(p Provider, plain bool) error {
	snap, _, _ := fetchSnapshotFn(p)
	ext := ReadExtenderStatus(p)
	var sigpipeRisk *bool
	if p.Unit != "" && p.Supervisor != "openrc" {
		if state, err := showSigpipeState(p, runSigpipeSystemctl); err == nil && state.risk() {
			t := true
			sigpipeRisk = &t
		}
	}
	rows := makeStatusRows(p, snap, ext, sigpipeRisk)

	opts := statusLiveOptsFn()
	if plain {
		opts.Color = false
		opts.ASCII = true
	}
	width := opts.Width
	if width <= 0 || width > 80 {
		width = 80
	}
	var buf bytes.Buffer
	if err := renderStatusRows(&buf, rows, width, opts.Color); err != nil {
		return err
	}
	fmt.Print(buf.String())
	return nil
}

// renderStatusRows clips each row independently, including on narrow terminals.
// Color is requested by the caller only after checking terminal and NO_COLOR.
func renderStatusRows(w io.Writer, rows []statusRow, width int, color bool) error {
	if width <= 0 || width > 80 {
		width = 80
	}
	for _, row := range rows {
		prefix := fmt.Sprintf("* %-9s ", row.Name)
		line := prefix + strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, row.Text)
		if utf8.RuneCountInString(line) > width {
			line = string([]rune(line)[:width])
		}
		if color && strings.HasPrefix(line, "*") {
			code := map[statusLevel]string{statusOK: "32", statusWarn: "33", statusBad: "31", statusUnknown: "90"}[row.Level]
			if code != "" {
				line = "\x1b[" + code + "m*\x1b[0m" + line[1:]
			}
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
