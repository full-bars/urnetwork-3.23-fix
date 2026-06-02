package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// proxyHealthListCap bounds the stdout/combined-log detail lines. It does NOT
// apply to the persistent files, which always carry the complete list.
const proxyHealthListCap = 50

// capProxyList joins items with ", ", truncating to cap with a "(+N more)" suffix.
func capProxyList(items []string, cap int) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) <= cap {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:cap], ", ") + fmt.Sprintf(", ... (+%d more)", len(items)-cap)
}

// formatStateFile renders the complete current-state snapshot (uncapped).
func formatStateFile(r connect.ProxyHealthReport, now time.Time) string {
	var b strings.Builder
	down := len(r.Dead) + len(r.Degraded)
	fmt.Fprintf(&b, "# updated %s  up=%d down=%d dead=%d degraded=%d\n",
		now.UTC().Format(time.RFC3339), r.Up, down, len(r.Dead), len(r.Degraded))
	fmt.Fprintf(&b, "# lifetime_recovered=%d lifetime_lost=%d\n", r.LifetimeRecovered, r.LifetimeLost)
	for _, s := range r.Dead {
		fmt.Fprintf(&b, "DEAD     %s\n", s)
	}
	for _, s := range r.Degraded {
		fmt.Fprintf(&b, "DEGRADED %s\n", s)
	}
	return b.String()
}

// formatEventLines renders one append-line per transition (complete, uncapped).
func formatEventLines(r connect.ProxyHealthReport, now time.Time) []string {
	ts := now.UTC().Format(time.RFC3339)
	var lines []string
	for _, e := range r.Recovered {
		if e.After > 0 {
			lines = append(lines, fmt.Sprintf("%s RECOVERED %s after=%s", ts, connectProxyEntry(e), e.After.Round(time.Second)))
		} else {
			lines = append(lines, fmt.Sprintf("%s RECOVERED %s", ts, connectProxyEntry(e)))
		}
	}
	for _, e := range r.NewlyDegraded {
		lines = append(lines, fmt.Sprintf("%s DEGRADED  %s", ts, connectProxyEntry(e)))
	}
	for _, e := range r.NewlyDead {
		lines = append(lines, fmt.Sprintf("%s DEAD      %s", ts, connectProxyEntry(e)))
	}
	return lines
}

func connectProxyEntry(e connect.ProxyEvent) string {
	return fmt.Sprintf("proxy[%d] (%s)", e.Index, e.Address)
}
