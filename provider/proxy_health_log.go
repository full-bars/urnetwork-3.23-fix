package main

import (
	"fmt"
	"os"
	"path/filepath"
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

const proxyHealthLogMaxBytes = 20 * 1024 * 1024 // 20 MB

// proxyHealthDir resolves the directory for the persistent files:
// URNETWORK_PROXY_HEALTH_DIR, else <home>/.urnetwork. Returns ok=false if neither
// can be resolved (persistence then disabled by the caller).
func proxyHealthDir() (string, bool) {
	if d := os.Getenv("URNETWORK_PROXY_HEALTH_DIR"); d != "" {
		return d, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".urnetwork"), true
}

// writeProxyHealthState atomically rewrites the current-state snapshot file.
func writeProxyHealthState(dir string, r connect.ProxyHealthReport, now time.Time) {
	path := filepath.Join(dir, "proxy_health.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatStateFile(r, now)), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// writeProxyHealthEvents appends transition lines (if any) to the event log,
// rotating first when it would exceed the size cap.
func writeProxyHealthEvents(dir string, r connect.ProxyHealthReport, now time.Time) {
	lines := formatEventLines(r, now)
	if len(lines) == 0 {
		return
	}
	path := filepath.Join(dir, "proxy_health.log")
	rotateIfNeeded(path, proxyHealthLogMaxBytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(lines, "\n") + "\n")
}

// rotateIfNeeded renames path to path.1 (replacing any prior .1) when it exceeds
// maxBytes, keeping one generation of history.
func rotateIfNeeded(path string, maxBytes int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}
