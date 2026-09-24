package urnettools

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/urnetwork/connect/internal/tui"
)

// liveOpts controls how the live block is drawn. The renderers are pure
// functions of (snapshot, opts) so they can be golden-tested.
type liveOpts struct {
	Color bool // emit ANSI colors
	ASCII bool // plain sparkline glyphs (dumb terminals)
	Width int  // terminal columns; <= 0 means "wide enough, never wrap"
}

const (
	liveLabelWidth = 10
	liveIndent     = 2 + liveLabelWidth + 2 // "  " + label + gap
	liveSegSep     = "   "
	liveSparkMax   = 30
	liveWideWidth  = 1 << 20
)

// liveOptsFromEnv derives drawing options for an output stream. Color needs a
// terminal and honors NO_COLOR; TERM=dumb drops both color and the block
// glyphs of the sparkline.
func liveOptsFromEnv(isTTY bool, width int) liveOpts {
	dumb := os.Getenv("TERM") == "dumb"
	return liveOpts{
		Color: isTTY && !dumb && os.Getenv("NO_COLOR") == "",
		ASCII: dumb,
		Width: width,
	}
}

// paint wraps s in an ANSI style when color is on.
func (o liveOpts) paint(code, s string) string {
	if !o.Color || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// stateColor maps a snapshot state to its ANSI code: flowing green, idle
// yellow, degraded red, everything else (starting, stopped) dim.
func stateColor(state string) string {
	switch state {
	case "flowing":
		return "32"
	case "idle":
		return "33"
	case "degraded":
		return "31"
	default:
		return "2"
	}
}

// formatBytes renders a byte count with a binary unit, one decimal.
func formatBytes(n float64) string {
	const k = 1024.0
	switch {
	case n < k:
		return fmt.Sprintf("%.0f B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", n/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", n/(k*k))
	case n < k*k*k*k:
		return fmt.Sprintf("%.1f GiB", n/(k*k*k))
	default:
		return fmt.Sprintf("%.1f TiB", n/(k*k*k*k))
	}
}

// snapshotRate renders a float bytes-per-second value with formatRate.
func snapshotRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	return formatRate(uint64(bps))
}

// formatUptime renders a duration as its two largest units: 3d 4h, 5h 12m,
// 7m 30s, 42s.
func formatUptime(seconds float64) string {
	s := int64(seconds)
	if s < 0 {
		s = 0
	}
	d, h, m := s/86400, s%86400/3600, s%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s%60)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// liveRow is one labeled line of the block, made of segments that wrap onto
// continuation lines (aligned under the first segment) when the terminal is
// too narrow.
type liveRow struct {
	label string
	sep   string
	segs  []string
}

// wrapRow lays a row out within width columns.
func wrapRow(r liveRow, width int) []string {
	if width <= 0 {
		width = liveWideWidth
	}
	sep := r.sep
	if sep == "" {
		sep = liveSegSep
	}
	prefix := fmt.Sprintf("  %-*s  ", liveLabelWidth, r.label)
	cont := strings.Repeat(" ", liveIndent)
	var lines []string
	cur, curW := prefix, 0
	for _, seg := range r.segs {
		w := utf8.RuneCountInString(seg)
		if curW > 0 && liveIndent+curW+utf8.RuneCountInString(sep)+w > width {
			lines = append(lines, strings.TrimRight(cur, " "))
			cur, curW = cont, 0
		}
		if curW > 0 {
			cur += sep
			curW += utf8.RuneCountInString(sep)
		}
		cur += seg
		curW += w
	}
	return append(lines, strings.TrimRight(cur, " "))
}

// renderLiveBlock draws the live section of `urnet-tools status`. Every line
// ends in a newline; the block starts with a header, no leading blank line.
func renderLiveBlock(s *NodeSnapshot, o liveOpts) string {
	var b strings.Builder
	state := strings.ToUpper(s.State)
	if state == "" {
		state = "UNKNOWN"
	}
	header := fmt.Sprintf("Live (provider %s)   %s", orDash(s.Version), o.paint(stateColor(s.State), state))
	if s.RestartPending {
		header += "  " + o.paint("33", "RESTART PENDING")
	}
	b.WriteString(header + "\n")

	emit := func(r liveRow) {
		for _, ln := range wrapRow(r, o.Width) {
			b.WriteString(ln + "\n")
		}
	}

	// throughput: now, 1m and 5m averages, and a sparkline of the history.
	rate := liveRow{label: "throughput", segs: []string{
		snapshotRate(s.Rate.NowBps),
		"1m " + snapshotRate(s.Rate.Avg1mBps),
		"5m " + snapshotRate(s.Rate.Avg5mBps),
	}}
	if n := len(s.Rate.HistoryBps); n > 0 {
		w := n
		if w > liveSparkMax {
			w = liveSparkMax
		}
		if o.Width > 0 && w > o.Width-liveIndent {
			w = o.Width - liveIndent
		}
		vals := make([]uint64, n)
		for i, v := range s.Rate.HistoryBps {
			if v > 0 {
				vals[i] = uint64(v)
			}
		}
		if w > 0 {
			rate.segs = append(rate.segs, tui.Spark(vals, w, o.ASCII))
		}
	}
	emit(rate)

	emit(liveRow{label: "clients", segs: []string{
		fmt.Sprintf("%d (sessions: %d pqe, %d classical)", s.Clients, s.Sessions.PQE, s.Sessions.Classical),
	}})

	emit(liveRow{label: "proxies", sep: ", ", segs: []string{
		fmt.Sprintf("%d up", s.Proxies.Up),
		fmt.Sprintf("%d degraded", s.Proxies.Degraded),
		fmt.Sprintf("%d connecting", s.Proxies.Connecting),
		fmt.Sprintf("%d dead", s.Proxies.Dead),
	}})

	up := liveRow{label: "uptime", segs: []string{formatUptime(s.UptimeSeconds)}}
	if lr := lastRestartText(s); lr != "" {
		up.segs = append(up.segs, lr)
	}
	emit(up)

	// Resources: fields the platform could not supply are left out.
	var mem []string
	r := s.Resources
	if r.HeapInuseBytes > 0 {
		heap := formatBytes(float64(r.HeapInuseBytes)) + " heap"
		if r.MemLimitBytes != nil {
			heap += " of " + formatBytes(float64(*r.MemLimitBytes)) + " limit"
		}
		mem = append(mem, heap)
	}
	if r.RSSBytes != nil {
		mem = append(mem, "RSS "+formatBytes(float64(*r.RSSBytes)))
	}
	if r.Goroutines > 0 {
		mem = append(mem, fmt.Sprintf("%d goroutines", r.Goroutines))
	}
	switch {
	case r.OpenFDs != nil && r.FDLimit != nil:
		mem = append(mem, fmt.Sprintf("%d of %d fds", *r.OpenFDs, *r.FDLimit))
	case r.OpenFDs != nil:
		mem = append(mem, fmt.Sprintf("%d fds", *r.OpenFDs))
	}
	if len(mem) > 0 {
		emit(liveRow{label: "memory", sep: ", ", segs: mem})
	}

	emit(liveRow{label: "pressure", segs: []string{fmt.Sprintf("%.2f", s.Pressure)}})

	if label, text, ok := s.whyRow(); ok {
		emit(liveRow{label: label, segs: []string{text}})
	}
	return b.String()
}

// lastRestartText renders "last restart: update (v1 to v2)"; the version pair
// appears only when the provider version changed across the restart.
func lastRestartText(s *NodeSnapshot) string {
	if s.Restart.Reason == "" {
		return ""
	}
	txt := "last restart: " + s.Restart.Reason
	if s.PreviousVersion != nil && *s.PreviousVersion != "" && *s.PreviousVersion != s.Version {
		txt += fmt.Sprintf(" (%s to %s)", *s.PreviousVersion, s.Version)
	}
	return txt
}

// providerRow is one line of the multi-provider summary. Snap is nil when the
// provider did not answer (stopped, or too old for the snapshot command).
type providerRow struct {
	Name    string
	Running bool
	Version string // fallback when there is no snapshot
	Snap    *NodeSnapshot
}

// renderProviderTable draws one compact row per provider: state, rate,
// clients, uptime, version.
func renderProviderTable(rows []providerRow, o liveOpts) string {
	header := []string{"PROVIDER", "STATE", "RATE", "CLIENTS", "UPTIME", "VERSION"}
	type cell struct{ text, code string }
	var body [][]cell
	for _, r := range rows {
		c := []cell{{r.Name, ""}}
		switch {
		case r.Snap != nil:
			s := r.Snap
			c = append(c,
				cell{strings.ToUpper(s.State), stateColor(s.State)},
				cell{snapshotRate(s.Rate.NowBps), ""},
				cell{fmt.Sprintf("%d", s.Clients), ""},
				cell{formatUptime(s.UptimeSeconds), ""},
				cell{orDash(s.Version), ""})
		case r.Running:
			c = append(c, cell{"RUNNING", "2"}, cell{"-", ""}, cell{"-", ""}, cell{"-", ""}, cell{orDash(r.Version), ""})
		default:
			c = append(c, cell{"STOPPED", "2"}, cell{"-", ""}, cell{"-", ""}, cell{"-", ""}, cell{orDash(r.Version), ""})
		}
		body = append(body, c)
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range body {
		for i, c := range r {
			if n := utf8.RuneCountInString(c.text); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	line := func(cells []cell) {
		for i, c := range cells {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text))
			if i == len(cells)-1 {
				pad = ""
			}
			b.WriteString(o.paint(c.code, c.text) + pad)
			if i < len(cells)-1 {
				b.WriteString("  ")
			}
		}
		b.WriteString("\n")
	}
	hc := make([]cell, len(header))
	for i, h := range header {
		hc[i] = cell{h, ""}
	}
	line(hc)
	for _, r := range body {
		line(r)
	}
	return b.String()
}
