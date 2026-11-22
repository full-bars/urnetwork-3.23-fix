package urnettools

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
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
		level := statusOK
		// A sweep still running (unchecked) or a proxy without UDP (no relay) is
		// normal and never grades the node down. Only a mismatch is a warning.
		if u.Mismatch > 0 {
			level = statusWarn
		}
		rows = append(rows, statusRow{"UDP", level, statusUDPText(u)})
	}
	extender := statusRow{"Extender", statusUnknown, "unavailable"}
	if e.Known {
		extender.Level, extender.Text = statusOK, "off"
		switch {
		case e.Running && extenderRoleUnavailable(e.RoleState):
			// the role exists but cannot serve, for example tcp 443 cannot be bound
			extender.Level, extender.Text = statusWarn, "unavailable"
		case e.Running:
			extender.Text = "active"
		case e.Activating:
			extender.Level, extender.Text = statusWarn, "activating"
		case e.Setting == "on":
			// switched on but no role is running: the start failed or is held
			extender.Level, extender.Text = statusWarn, "not running"
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

// statusUDPText is the UDP row of the summary. It stays inside 80 columns and
// leaves out counts that are zero, so the percentage is never clipped away.
func statusUDPText(u *SnapshotProxyUDP) string {
	checked := u.OK + u.NoRelay + u.Mismatch + u.Unknown
	parts := []string{fmt.Sprintf("%d ok", u.OK)}
	for _, c := range []struct {
		n    int
		name string
	}{{u.NoRelay, "no UDP"}, {u.Mismatch, "mismatch"}, {u.Unknown, "unknown"}, {u.Unchecked, "unchecked"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.name))
		}
	}
	text := strings.Join(parts, ", ")
	if checked > 0 {
		text += fmt.Sprintf(" (%d%% of checked)", u.OK*100/checked)
	}
	return text
}

// formatStatusSummary formats the one-screen summary into a string buffer:
// the update line and the summary rows. plain disables color and escape sequences.
func formatStatusSummary(d statusData, plain bool) (string, error) {
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
	main, hint := statusUpdateParts(d.Provider.Version, d.Update)
	fmt.Fprintln(&buf, statusClip(main, width))
	if hint != "" {
		fmt.Fprintf(&buf, "           %s\n", statusClip(hint, width-11))
	}
	if err := renderStatusRows(&buf, d.Rows, width, opts.Color); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderStatusSummary renders the one-screen status summary for one provider:
// the update line and the rows from the summary model. plain disables color
// and escape sequences.
func renderStatusSummary(p Provider, plain bool) error {
	d := collectStatusData(p)
	text, err := formatStatusSummary(d, plain)
	if err != nil {
		return err
	}
	fmt.Print(text)
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
// the live snapshot the rows were built from, the rows, the update state, and
// the exit code.
type statusData struct {
	Provider Provider
	Snapshot *NodeSnapshot
	Rows     []statusRow
	Update   statusUpdateState
	ExitCode int
}

// statusSigpipeStateFn reads the unit's restart settings. It is a variable so a
// test does not run the box's systemctl.
var statusSigpipeStateFn = func(p Provider) (sigpipeState, error) {
	return showSigpipeState(p, runSigpipeSystemctl)
}

var statusLingerCheckFn = func(user string) (bool, error) {
	out, err := execWithTimeout(5*time.Second, "loginctl", "show-user", user, "-p", "Linger", "--value")
	return strings.TrimSpace(string(out)) == "yes", err
}

func statusLingerApplicable(p Provider) bool {
	return runtime.GOOS == "linux" && p.Unit != "" && p.Supervisor != "openrc" && p.User != "" && isUserUnit(p.Unit)
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
		// A reading that succeeded and found no risk is a result: the unit row
		// reads ok. Only a failed reading leaves the row unknown.
		if state, err := statusSigpipeStateFn(p); err == nil {
			risk := state.risk()
			sigpipeRisk = &risk
		}
	}
	rows := makeStatusRows(p, snap, ext, sigpipeRisk)
	if statusLingerApplicable(p) {
		if enabled, err := statusLingerCheckFn(p.User); err == nil && !enabled {
			rows = append(rows, statusRow{"Linger", statusWarn, "off; run sudo loginctl enable-linger " + p.User})
		}
	}
	updateState := checkStatusUpdateFn(p.StateDir)
	d := statusData{Provider: p, Snapshot: snap, Rows: rows, Update: updateState}
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

// statusBriefLine renders the one-line contract for scripts and monitors,
// for example `ok v3.23.0-fix.35.11 900/900 proxies extender active`.
func statusBriefLine(d statusData) string {
	// The word follows the exit code, so the text and the code a script reads
	// can never disagree.
	word := map[int]string{0: "ok", 1: "warn", 2: "bad", 3: "unreachable"}[statusExitCode(d)]
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

// extenderRoleUnavailable reports whether a running extender role is in a state
// where it cannot serve clients.
func extenderRoleUnavailable(state string) bool {
	switch state {
	case "tcp_unavailable", "unavailable", "error", "closed":
		return true
	}
	return false
}
