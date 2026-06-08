package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// proxyHealthListCap bounds the stdout/combined-log detail lines. It does NOT
// apply to the persistent files, which always carry the complete list.
const proxyHealthListCap = 50

// capProxyList extracts the proxy index from items, joins them with ", ", and truncates to cap with a "(+N more)" suffix.
func capProxyList(items []string, cap int) string {
	if len(items) == 0 {
		return ""
	}
	var shortItems []string
	for _, item := range items {
		start := strings.Index(item, "[")
		end := strings.Index(item, "]")
		if start != -1 && end != -1 && end > start {
			shortItems = append(shortItems, item[start+1:end])
		} else {
			shortItems = append(shortItems, item)
		}
	}
	
	if len(shortItems) <= cap {
		return strings.Join(shortItems, ", ")
	}
	return strings.Join(shortItems[:cap], ", ") + fmt.Sprintf(", ... (+%d more)", len(shortItems)-cap)
}

func parseProxyString(s string) (string, string) {
	parts := strings.SplitN(s, " (", 2)
	if len(parts) == 2 {
		return parts[0], strings.TrimRight(parts[1], ")")
	}
	return s, ""
}

// formatStateFile renders the complete current-state snapshot (uncapped).
func formatStateFile(r connect.ProxyHealthReport, now time.Time) string {
	var b strings.Builder
	down := len(r.Dead) + len(r.Degraded)
	fmt.Fprintf(&b, "=========================================================================\n")
	fmt.Fprintf(&b, " URNETWORK PROXY HEALTH REPORT\n")
	fmt.Fprintf(&b, " Updated: %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, " Up: %d | Down: %d | Dead: %d | Degraded: %d\n", r.Up, down, len(r.Dead), len(r.Degraded))
	fmt.Fprintf(&b, " Lifetime Recovered: %d | Lifetime Lost: %d\n", r.LifetimeRecovered, r.LifetimeLost)
	fmt.Fprintf(&b, "=========================================================================\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+\n")
	fmt.Fprintf(&b, "| STATUS   | PROXY ID         | IP ADDRESS                              |\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+\n")
	for _, s := range r.Dead {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s |\n", "DEAD", p, ip)
	}
	for _, s := range r.Degraded {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s |\n", "DEGRADED", p, ip)
	}
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+\n")
	return b.String()
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := float64(unit), 0
	for n := float64(b) / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/div, "KMGTPE"[exp])
}

func formatAgeDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

// formatTrafficStateFile renders the complete traffic usage report.
func formatTrafficStateFile(r connect.ProxyHealthReport, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "======================================================================================================\n")
	fmt.Fprintf(&b, " URNETWORK PROXY TRAFFIC REPORT\n")
	fmt.Fprintf(&b, " Updated: %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "======================================================================================================\n")
	fmt.Fprintf(&b, "+------------------+-----------------------+---------+---------+-------------------+-------------------+\n")
	fmt.Fprintf(&b, "| PROXY ID         | IP ADDRESS            | CLIENTS | MAX AGE | BILLABLE (TX/RX)  | TOTAL (TX/RX)     |\n")
	fmt.Fprintf(&b, "+------------------+-----------------------+---------+---------+-------------------+-------------------+\n")

	type proxyEntry struct {
		Proxy string
		Index int
		IP    string
		Bw    *connect.ProxyBandwidth
	}
	var entries []proxyEntry
	for k, bw := range r.Bandwidth {
		p, ip := parseProxyString(k)
		var index int
		fmt.Sscanf(p, "proxy[%d]", &index)
		entries = append(entries, proxyEntry{Proxy: p, Index: index, IP: ip, Bw: bw})
	}
	sort.Slice(entries, func(i, j int) bool {
		sumI := entries[i].Bw.BillableTx.Load() + entries[i].Bw.BillableRx.Load()
		sumJ := entries[j].Bw.BillableTx.Load() + entries[j].Bw.BillableRx.Load()
		if sumI != sumJ {
			return sumI > sumJ // descending order
		}
		return entries[i].Index < entries[j].Index
	})

	for _, e := range entries {
		billableStr := fmt.Sprintf("%s / %s", formatBytes(e.Bw.BillableTx.Load()), formatBytes(e.Bw.BillableRx.Load()))
		totalStr := fmt.Sprintf("%s / %s", formatBytes(e.Bw.TotalTx.Load()), formatBytes(e.Bw.TotalRx.Load()))
		fmt.Fprintf(&b, "| %-16s | %-21s | %-7d | %-7s | %-17s | %-17s |\n", e.Proxy, e.IP, e.Bw.Clients.Load(), formatAgeDuration(e.Bw.MaxAge()), billableStr, totalStr)
	}

	return b.String()
}

// formatEventLines renders one append-line per transition (complete, uncapped).
func formatEventLines(r connect.ProxyHealthReport, now time.Time) []string {
	ts := now.UTC().Format(time.RFC3339)
	var lines []string
	for _, e := range r.Recovered {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		if e.After > 0 {
			lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | after=%-7s |", ts, "RECOVERED", p, e.Address, e.After.Round(time.Second)))
		} else {
			lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "RECOVERED", p, e.Address, ""))
		}
	}
	for _, e := range r.NewlyDegraded {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "DEGRADED", p, e.Address, ""))
	}
	for _, e := range r.NewlyDead {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "DEAD", p, e.Address, ""))
	}
	return lines
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

// writeProxyTrafficState atomically rewrites the proxy traffic snapshot file.
func writeProxyTrafficState(dir string, r connect.ProxyHealthReport, now time.Time) {
	path := filepath.Join(dir, "proxy_traffic.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatTrafficStateFile(r, now)), 0644); err != nil {
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
