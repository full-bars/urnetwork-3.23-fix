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
// meter draws a short filled bar for a ratio between 0 and 1. It only draws on
// color terminals, so plain and dumb output keep their exact text. goodHigh
// says whether a full bar is good news (coverage) or bad news (memory).
func (o liveOpts) meter(ratio float64, goodHigh bool) string {
	if !o.Color || o.ASCII || ratio != ratio {
		return ""
	}
	const cells = 12
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	fill := int(ratio*cells + 0.5)
	good := ratio
	if !goodHigh {
		good = 1 - ratio
	}
	code := "32"
	switch {
	case good < 0.25:
		code = "31"
	case good < 0.5:
		code = "33"
	}
	return o.paint(code, strings.Repeat("█", fill)) + o.paint("90", strings.Repeat("░", cells-fill))
}

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
// visibleWidth counts the characters a terminal shows: color escape sequences
// take no columns.
func visibleWidth(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

// withMeter puts a meter after the row's last segment, set apart by a gap.
func withMeter(r liveRow, m string) liveRow {
	if m != "" && len(r.segs) > 0 {
		r.segs[len(r.segs)-1] += "  " + m
	}
	return r
}

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
		w := visibleWidth(seg)
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
// pressureSummary, when non-empty, is the provider's persisted one-sentence
// pressure summary: it replaces the bare score in the pressure row (the
// sentence carries the score too).
func renderLiveBlock(s *NodeSnapshot, o liveOpts, pressureSummary string) string {
	var b strings.Builder
	state := strings.ToUpper(s.State)
	if state == "" {
		state = "UNKNOWN"
	}
	header := fmt.Sprintf("%s (provider %s)   %s", o.paint("1;36", "Live"), orDash(s.Version), o.paint(stateColor(s.State), state))
	if s.RestartPending {
		header += "  " + o.paint("33", "RESTART PENDING")
	}
	b.WriteString(header + "\n")

	emit := func(r liveRow) {
		for _, ln := range wrapRow(r, o.Width) {
			b.WriteString(ln + "\n")
		}
	}

	// A rate row: now, 1m and 5m averages, and a sparkline of the history.
	rateRow := func(label string, now, avg1m, avg5m float64, history []float64) liveRow {
		row := liveRow{label: label, segs: []string{
			snapshotRate(now),
			"1m " + snapshotRate(avg1m),
			"5m " + snapshotRate(avg5m),
		}}
		if n := len(history); n > 0 {
			w := n
			if w > liveSparkMax {
				w = liveSparkMax
			}
			if o.Width > 0 && w > o.Width-liveIndent {
				w = o.Width - liveIndent
			}
			vals := make([]uint64, n)
			for i, v := range history {
				if v > 0 {
					vals[i] = uint64(v)
				}
			}
			if w > 0 {
				row.segs = append(row.segs, tui.Spark(vals, w, o.ASCII))
			}
		}
		return row
	}

	// Billable is what earns; total is everything moved. A provider that predates
	// the traffic block shows the one row it always did, labeled as before.
	if tr := s.Traffic; tr != nil {
		emit(rateRow("billable", s.Rate.NowBps, s.Rate.Avg1mBps, s.Rate.Avg5mBps, s.Rate.HistoryBps))
		emit(rateRow("total", tr.TotalNowBps, tr.TotalAvg1mBps, tr.TotalAvg5mBps, tr.TotalHistoryBps))
		moved := []string{
			formatBytes(float64(tr.BillableBytes)) + " billable",
			formatBytes(float64(tr.TotalBytes)) + " total (this run)",
		}
		if tr.LifetimeBillableBytes != nil {
			moved = append(moved, formatBytes(float64(*tr.LifetimeBillableBytes))+" billable lifetime")
		}
		emit(liveRow{label: "moved", sep: ", ", segs: moved})
	} else {
		emit(rateRow("throughput", s.Rate.NowBps, s.Rate.Avg1mBps, s.Rate.Avg5mBps, s.Rate.HistoryBps))
	}

	emit(liveRow{label: "clients", segs: []string{
		fmt.Sprintf("%d (sessions: %d pqe, %d classical)", s.Clients, s.Sessions.PQE, classicalSessions(s.Clients, s.Sessions.PQE, s.Sessions.Classical)),
	}})

	proxyRow := liveRow{label: "proxies", sep: ", ", segs: []string{
		fmt.Sprintf("%d up", s.Proxies.Up),
		fmt.Sprintf("%d dropped", s.Proxies.Degraded),
		fmt.Sprintf("%d connecting", s.Proxies.Connecting),
		fmt.Sprintf("%d dead", s.Proxies.Dead),
	}}
	if n := s.Proxies.Up + s.Proxies.Degraded + s.Proxies.Connecting + s.Proxies.Dead; n > 0 {
		proxyRow = withMeter(proxyRow, o.meter(float64(s.Proxies.Up)/float64(n), true))
	}
	emit(proxyRow)

	// UDP coverage of online proxies (up or degraded). Older providers may
	// omit it.
	if s.Proxies.UDP != nil {
		udpRow := udprow(s.Proxies.UDP)
		if u := s.Proxies.UDP; u.OK+u.NoRelay+u.Mismatch+u.Unknown+u.Unchecked > 0 {
			online := u.OK + u.NoRelay + u.Mismatch + u.Unknown + u.Unchecked
			udpRow = withMeter(udpRow, o.meter(float64(u.OK)/float64(online), true))
		}
		emit(udpRow)
	}

	// IPv6 coverage of online proxies.
	if s.Proxies.IPv6 != nil {
		emit(ipv6row(s.Proxies.IPv6))
	}

	// Trim: only when a cap is in force, and only when the provider reported
	// one. The configured count is the pool size before the cap, so the row
	// says how much capacity the cap is holding back.
	if s.Proxies.TrimCap > 0 || (s.Proxies.Parked != nil && *s.Proxies.Parked > 0) {
		segs := []string{}
		if s.Proxies.Parked != nil {
			segs = append(segs, fmt.Sprintf("%d parked", *s.Proxies.Parked))
		}
		if s.Proxies.TrimCap > 0 {
			segs = append(segs, fmt.Sprintf("holding at %d running", s.Proxies.TrimCap))
		}
		if s.Proxies.Configured > 0 {
			segs = append(segs, fmt.Sprintf("%d configured", s.Proxies.Configured))
		}
		if src := trimSourceLabel(s.Proxies.TrimCapSource); src != "" {
			segs = append(segs, src)
		}
		emit(liveRow{label: "trim", sep: ", ", segs: segs})
	}

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
		memRow := liveRow{label: "memory", sep: ", ", segs: mem}
		if r.HeapInuseBytes > 0 && r.MemLimitBytes != nil && *r.MemLimitBytes > 0 {
			memRow = withMeter(memRow, o.meter(float64(r.HeapInuseBytes)/float64(*r.MemLimitBytes), false))
		}
		emit(memRow)
	}

	// Plain language first: when the provider persists its summary sentence
	// (every monitor tick), the row shows the sentence instead of the raw
	// number. Older providers that never wrote the file keep the bare score.
	pressureSegs := []string{fmt.Sprintf("%.2f", s.Pressure)}
	if pressureSummary != "" {
		// Fold the sentence's few non-ASCII runes on plain terminals: the
		// provider writes an em dash in elevated sentences, and TERM=dumb
		// output must stay <= 127 runes per the block's contract.
		if o.ASCII {
			pressureSummary = strings.Map(func(r rune) rune {
				switch r {
				case '—', '–':
					return '-'
				case '‘', '’':
					return '\''
				case '“', '”':
					return '"'
				case '…':
					return '.'
				case 0xA0: // no-break space
					return ' '
				}
				if r > 127 {
					return '?'
				}
				return r
			}, pressureSummary)
		}
		// Split into word segments so wrapRow can fold a long summary at any
		// word boundary instead of hard-overflowing the terminal (sentences
		// run 75-110+ chars, wider than 80 columns, and a single clause can
		// too). The words re-join with single spaces, so wide terminals see
		// the exact same sentence.
		pressureSegs = strings.Fields(pressureSummary)
		if len(pressureSegs) == 0 {
			// Defensive: a whitespace-only summary must fall back to the
			// score instead of rendering an empty pressure row.
			pressureSegs = []string{fmt.Sprintf("%.2f", s.Pressure)}
		}
	}
	if m := o.meter(s.Pressure, false); m != "" {
		pressureSegs[len(pressureSegs)-1] += "  " + m
	}
	emit(liveRow{label: "pressure", sep: " ", segs: pressureSegs})

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

// udprow renders the UDP coverage row for the live block. Percentages use
// integer division; both show "n/a" when their denominator is zero.
func udprow(u *SnapshotProxyUDP) liveRow {
	checked := u.OK + u.NoRelay + u.Mismatch + u.Unknown
	online := checked + u.Unchecked
	segs := []string{
		fmt.Sprintf("%d ok", u.OK),
		fmt.Sprintf("%d no UDP", u.NoRelay),
		fmt.Sprintf("%d mismatch", u.Mismatch),
		fmt.Sprintf("%d unchecked", u.Unchecked),
	}
	if u.Unknown > 0 {
		segs = append(segs, fmt.Sprintf("%d unknown", u.Unknown))
	}
	var pct string
	switch {
	case checked > 0 && online > 0:
		pct = fmt.Sprintf("(%d%% of checked, %d%% of online)", u.OK*100/checked, u.OK*100/online)
	case checked > 0:
		pct = fmt.Sprintf("(%d%% of checked, n/a of online)", u.OK*100/checked)
	case online > 0:
		pct = "(n/a of checked, 0% of online)"
	default:
		pct = "(n/a of checked, n/a of online)"
	}
	segs = append(segs, pct)
	return liveRow{label: "udp", sep: ", ", segs: segs}
}

// ipv6row renders the IPv6 capability row for the live block.
func ipv6row(v6 *SnapshotProxyIPv6) liveRow {
	segs := []string{
		fmt.Sprintf("%d yes", v6.Yes),
		fmt.Sprintf("%d no", v6.No),
		fmt.Sprintf("%d unknown", v6.Unknown),
	}
	if v6.NotProbed > 0 {
		segs = append(segs, fmt.Sprintf("%d not probed", v6.NotProbed))
	}
	// the UDP summary is one segment per count, so wrapRow can break it at a
	// separator. Joined with ", " it reads as one parenthesis.
	udpParts := []string{fmt.Sprintf("%d yes", v6.UDPYes)}
	if v6.UDPNo > 0 {
		udpParts = append(udpParts, fmt.Sprintf("%d no", v6.UDPNo))
	}
	if v6.UDPUnknown > 0 {
		udpParts = append(udpParts, fmt.Sprintf("%d unknown", v6.UDPUnknown))
	}
	udpParts[0] = "(udp: " + udpParts[0]
	udpParts[len(udpParts)-1] += ")"
	segs = append(segs, udpParts...)
	return liveRow{label: "ipv6", sep: ", ", segs: segs}
}
