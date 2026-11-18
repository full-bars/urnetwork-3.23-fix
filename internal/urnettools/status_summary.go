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

// statusData is the complete, renderable state of the summary: the provider,
// the live snapshot the rows were built from, the rows, and the exit code.
type statusData struct {
	Provider Provider
	Snapshot *NodeSnapshot
	Rows     []statusRow
	ExitCode int
}

// collectStatusData reads every source without letting one slow source stop
// another. The snapshot stays nil when the provider does not answer, which
// drives both the "live status unavailable" rows and the unreachable exit
// code.
func collectStatusData(p Provider) statusData {
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
	d := statusData{Provider: p, Snapshot: snap, Rows: rows}
	d.ExitCode = statusExitCode(d)
	return d
}

// statusExitCode maps the summary to the documented script contract: 0
// healthy, 1 warning, 2 broken, 3 unreachable. A row that stays unknown, for
// example the SIGPIPE check on a platform that cannot run it, is not a fault
// by itself; only a missing live snapshot means unreachable.
func statusExitCode(d statusData) int {
	worst := statusOK
	for _, row := range d.Rows {
		switch row.Level {
		case statusBad:
			return 2
		case statusWarn:
			worst = statusWarn
		}
	}
	if d.Snapshot == nil {
		return 3
	}
	if worst == statusWarn {
		return 1
	}
	return 0
}

// worstLevel returns the worst row level, ignoring unknown rows.
func worstLevel(d statusData) statusLevel {
	worst := statusOK
	for _, row := range d.Rows {
		switch row.Level {
		case statusBad:
			return statusBad
		case statusWarn:
			worst = statusWarn
		}
	}
	return worst
}

// statusBriefLine renders the one-line contract for scripts and monitors,
// for example `ok v3.23.0-fix.35.11 900/900 proxies extender active`.
func statusBriefLine(d statusData) string {
	word := "ok"
	switch {
	case d.Snapshot == nil:
		word = "unreachable"
	case worstLevel(d) == statusBad:
		word = "bad"
	case worstLevel(d) == statusWarn:
		word = "warn"
	}
	version := firstSnapshotVersion(d)
	if version == "" {
		version = d.Provider.Version
	}
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("%s %s %s", word, version, statusBriefTail(d))
}

func firstSnapshotVersion(d statusData) string {
	for _, row := range d.Rows {
		if row.Name == "Version" {
			if row.Text == "unknown" {
				// Fall back to the provider's stamped version rather than
				// printing "unknown" for the whole line.
				continue
			}
			return row.Text
		}
	}
	return ""
}

func statusBriefTail(d statusData) string {
	var parts []string
	for _, row := range d.Rows {
		if row.Name == "Proxies" {
			parts = append(parts, statusBriefProxies(row.Text))
		}
		if row.Name == "Extender" {
			parts = append(parts, "extender "+statusBriefExtender(row.Text))
		}
	}
	return strings.Join(parts, " ")
}

// statusBriefProxies turns "58 up, 2 degraded, 0 connecting, 0 dead, trim
// 12/20" into "58/60 proxies".
func statusBriefProxies(text string) string {
	var up, degraded, connecting, dead int
	fmt.Sscanf(text, "%d up, %d degraded, %d connecting, %d dead", &up, &degraded, &connecting, &dead)
	total := up + degraded + connecting + dead
	if total == 0 {
		return "0/0 proxies"
	}
	return fmt.Sprintf("%d/%d proxies", up, total)
}

func statusBriefExtender(text string) string {
	switch text {
	case "active":
		return "active"
	case "off":
		return "off"
	case "activating":
		return "activating"
	default:
		return "unavailable"
	}
}
