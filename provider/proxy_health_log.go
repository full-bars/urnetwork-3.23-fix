package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// usageHistoryMaxBytes bounds the accumulated usage history file before it
// rotates to <name>.1. At ~90 bytes/row and hourly snapshots, 512 MiB is
// ~60+ years of history. The cap guards against a runaway writer. NOTE: if
// the file rotates, readUsageHistory still reads the rotated .1 file, so
// LIFETIME (segment-summed over the full history, see usageLifetime)
// continues to include pre-rotation rows.
const usageHistoryMaxBytes = 512 << 20 // 512 MiB

// usageHistoryPath is the persistent JSONL history of aggregate usage
// snapshots. One JSON object per line:
//
//	{"ts":"2026-08-31T05:00:00Z","rx":...,"tx":...,"billable_rx":...,"billable_tx":...}
//
// rx/tx are the cumulative TOTAL bytes across all proxies since the process
// started; billable_* are the cumulative billable bytes. The totals can dip on
// ordinary proxy churn (a removed proxy's bytes leave the aggregate sum), not
// just on a provider restart. Lifetime is computed by segment-summing the
// history (usageLifetime); "since start" is the latest snapshot's values.
func usageHistoryPath(dir string) string {
	return filepath.Join(dir, "usage_history.jsonl")
}

// usageHistoryLastHour tracks the last hour bucket we appended, so the health
// heartbeat (which fires far more often than hourly) only wires one snapshot
// per hour. Package-level so provider restarts within the same hour don't
// double-append startup rows.
var (
	usageHistoryMu       sync.Mutex
	usageHistoryLastHour int64 // UTC hour magnitude: unix/3600
)

// writeUsageHistory appends an aggregate usage snapshot if we haven't written
// one for the current UTC hour bucket. Called from the health heartbeat with
// the freshly-built ProxyHealthReport.
func writeUsageHistory(dir string, r connect.ProxyHealthReport, now time.Time) {
	var billableRx, billableTx, totalRx, totalTx uint64
	for _, bw := range r.Bandwidth {
		billableRx += bw.BillableRx.Load()
		billableTx += bw.BillableTx.Load()
		totalRx += bw.TotalRx.Load()
		totalTx += bw.TotalTx.Load()
	}

	hour := now.Unix() / 3600
	usageHistoryMu.Lock()
	defer usageHistoryMu.Unlock()
	if usageHistoryLastHour == hour {
		return // already appended this hour
	}
	usageHistoryLastHour = hour

	row := struct {
		TS         time.Time `json:"ts"`
		RX         uint64    `json:"rx"`
		TX         uint64    `json:"tx"`
		BillableRX uint64    `json:"billable_rx"`
		BillableTX uint64    `json:"billable_tx"`
	}{
		TS: now.UTC(), RX: totalRx, TX: totalTx, BillableRX: billableRx, BillableTX: billableTx,
	}
	b, err := json.Marshal(row)
	if err != nil {
		return
	}

	path := usageHistoryPath(dir)
	rotateIfNeeded(path, usageHistoryMaxBytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

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
	fmt.Fprintf(&b, "=======================================================================\n")
	fmt.Fprintf(&b, " URNETWORK PROXY HEALTH REPORT\n")
	fmt.Fprintf(&b, " Updated: %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, " Up: %d | Down: %d | Dead: %d | Degraded: %d\n", r.Up, down, len(r.Dead), len(r.Degraded))
	fmt.Fprintf(&b, " Lifetime Recovered: %d | Lifetime Lost: %d\n", r.LifetimeRecovered, r.LifetimeLost)
	fmt.Fprintf(&b, "=======================================================================\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
	fmt.Fprintf(&b, "| STATUS   | PROXY ID         | IP ADDRESS                              | AUTH ERRS  |\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
	for _, s := range r.Dead {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s | %-10s |\n", "DEAD", p, ip, "")
	}
	for _, s := range r.Degraded {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s | %-10s |\n", "DEGRADED", p, ip, "")
	}
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
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

// proxyHealthLogMu serialises every append and rotation of proxy_health.log.
// Two independent writers touch that file: the retention writer (a long-lived
// goroutine holding an open handle) and writeProxyHealthEvents (the heartbeat).
// Without a shared lock, a rotation by one renames the file out from under the
// other's open descriptor, and the loser's subsequent writes land in the renamed
// generation; the next rotation then overwrites .1 and those events are gone.
var proxyHealthLogMu sync.Mutex

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
	proxyHealthLogMu.Lock()
	defer proxyHealthLogMu.Unlock()
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

// retentionEventBuffer bounds the pending retention events before the writer
// drops new entries under sustained backpressure (events are telemetry, not
// correctness-critical — dropping is preferable to blocking the transfer
// sequence goroutine that fires the callback).
const retentionEventBuffer = 256

// retentionEventCh is the buffered channel feeding the single retention-event
// writer goroutine. Created lazily on first use so CLI subcommands that never
// wire RetentionEventCallback pay nothing.
var (
	retentionEventOnce sync.Once
	retentionEventCh   chan string
	retentionEventDone chan struct{}

	// retentionEventMu guards retentionEventClosed against the race between
	// flushRetentionEvents closing retentionEventCh and a SendSequence still
	// draining on shutdown calling appendRetentionEvent concurrently — the
	// teardown drain (transfer.go Run() exit path) fires RetentionEventCallback
	// for every retained item, which is exactly the same window flush runs in.
	// Without this guard a send on the closed channel panics the process
	// during what should be a graceful shutdown.
	retentionEventMu     sync.Mutex
	retentionEventClosed bool

	// retentionEventDropped counts events dropped because the buffer was
	// full. Incremented with a non-blocking atomic op on the hot path;
	// reported from the writer goroutine (off the hot path) instead of via
	// a synchronous stderr write at drop time, which could block on a slow
	// pipe/log collector and stall the transfer sequence that called
	// appendRetentionEvent.
	retentionEventDropped atomic.Uint64
)

// retentionEventBatch is how many buffered events the writer drains per file
// write. Draining one at a time cost a full open/stat/write/close cycle PER
// EVENT, so on a slow disk the single writer could not keep up with the event
// rate and the 256-slot buffer went permanently full, dropping most of the
// retention log on a large box. Batching turns draining a full buffer into one
// open and one write.
const retentionEventBatch = 64

// startRetentionEventWriter launches the single goroutine that performs the
// retention file I/O off the hot path. It takes the first event blocking, then
// drains whatever else is already queued without waiting, so a burst becomes
// one write while an idle stream still costs nothing.
func startRetentionEventWriter() {
	retentionEventOnce.Do(func() {
		retentionEventCh = make(chan string, retentionEventBuffer)
		retentionEventDone = make(chan struct{})
		go func() {
			defer close(retentionEventDone)
			w := newRetentionLogWriter()
			defer w.close()
			var lastReportedDropped uint64
			reportDrops := func() {
				if dropped := retentionEventDropped.Load(); dropped != lastReportedDropped {
					fmt.Fprintf(os.Stderr, "[provider] warn: retention event buffer was full, dropped %d event(s)\n", dropped-lastReportedDropped)
					lastReportedDropped = dropped
				}
			}
			batch := make([]string, 0, retentionEventBatch)
			for event := range retentionEventCh {
				batch = append(batch[:0], event)
				// Take whatever else is already queued, without blocking. A
				// closed channel drains to what is left and then yields ok=false,
				// so the outer range ends on the next turn.
			drain:
				for len(batch) < retentionEventBatch {
					select {
					case next, ok := <-retentionEventCh:
						if !ok {
							w.write(batch)
							reportDrops()
							return
						}
						batch = append(batch, next)
					default:
						break drain
					}
				}
				w.write(batch)
				reportDrops()
			}
		}()
	})
}

// retentionLogWriter appends retention lines to proxy_health.log, keeping the
// file open across batches. The handle is reopened when the file is rotated:
// renaming a path that is still open would leave every later append going to
// the rotated generation, so the writer must notice the rename and reopen.
type retentionLogWriter struct {
	path   string
	f      *os.File
	bytes  int64 // size at the last check, for the rotation threshold
	opened bool
}

func newRetentionLogWriter() *retentionLogWriter { return &retentionLogWriter{} }

// write appends a batch of events as one write.
func (w *retentionLogWriter) write(events []string) {
	if len(events) == 0 {
		return
	}
	dir, ok := proxyHealthDir()
	if !ok {
		return
	}
	proxyHealthLogMu.Lock()
	defer proxyHealthLogMu.Unlock()
	// The other writer (writeProxyHealthEvents) rotates by renaming the file
	// under this lock, which leaves an open handle here pointing at the rotated
	// generation while the path string is unchanged. Compare the handle with
	// what the path names NOW, and reopen when they differ. The live size is
	// re-read at the same time, because the other writer's appends never pass
	// through w.bytes.
	if size, live := w.liveSize(filepath.Join(dir, "proxy_health.log")); live {
		w.bytes = size
	} else {
		w.reopen(dir)
		if w.f == nil {
			return
		}
	}
	// Rotate BEFORE writing, when the previous batch already filled the file.
	// Checking after the write (as an earlier version did, despite its own
	// comment) puts the batch that crosses the threshold into the generation
	// that is about to be renamed, so the newest events are the ones lost from
	// the live file.
	//
	// w.bytes is the live size as just read (or, after a reopen, seeded from the
	// file), incremented by exactly what this batch appends.
	if w.bytes >= proxyHealthLogMaxBytes {
		w.close()
		if _, err := os.Stat(w.path); err == nil {
			_ = os.Rename(w.path, w.path+".1")
		}
		w.bytes = 0
		if w.f == nil {
			w.reopen(filepath.Dir(w.path))
			if w.f == nil {
				return
			}
		}
	}
	var b strings.Builder
	ts := time.Now().UTC().Format(time.RFC3339)
	for _, e := range events {
		fmt.Fprintf(&b, "| %s | %-9s | %-16s | %-21s | %s |\n", ts, "RETAIN", "-", "", e)
	}
	n, err := w.f.WriteString(b.String())
	if err != nil {
		// The handle went bad (rotation by the OTHER writer, truncation, disk).
		// Drop it so the next batch reopens rather than writing into a dead
		// file forever.
		w.close()
		return
	}
	w.bytes += int64(n)
}

// liveSize reports whether the open handle is still the file path names, and
// that file's current size. It is false when there is no handle, the handle was
// opened for a different path, the path no longer exists, or the path now names
// a different file (the other writer rotated it). The caller holds
// proxyHealthLogMu.
func (w *retentionLogWriter) liveSize(path string) (int64, bool) {
	if w.f == nil || w.path != path {
		return 0, false
	}
	handle, err := w.f.Stat()
	if err != nil {
		return 0, false
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(handle, current) {
		return 0, false
	}
	return current.Size(), true
}

func (w *retentionLogWriter) reopen(dir string) {
	w.close()
	w.path = filepath.Join(dir, "proxy_health.log")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	w.f = f
	w.opened = true
	if info, err := f.Stat(); err == nil {
		w.bytes = info.Size()
	} else {
		w.bytes = 0
	}
}

func (w *retentionLogWriter) close() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.opened = false
}

// appendRetentionEvent buffers a retention telemetry line and returns
// immediately; a single writer goroutine performs the file I/O. When the
// buffer is full the event is dropped (with a stderr note) rather than
// blocking the caller, which runs on the transfer sequence hot path.
func appendRetentionEvent(event string) {
	startRetentionEventWriter()
	retentionEventMu.Lock()
	defer retentionEventMu.Unlock()
	if retentionEventClosed {
		// Lost the race with shutdown's flush; the writer goroutine has
		// already been told to drain and exit. Telemetry, not correctness —
		// drop rather than reopen a closed channel.
		return
	}
	select {
	case retentionEventCh <- event:
	default:
		// Non-blocking: never write to stderr synchronously here. This runs
		// on the transfer sequence hot path (RetentionEventCallback), and a
		// blocked stderr write (slow pipe/log collector) would stall
		// transfer sequences under sustained retention-event backpressure.
		// The writer goroutine reports the drop count once it next runs.
		retentionEventDropped.Add(1)
	}
}

// flushRetentionEvents drains all buffered events and waits for the writer
// goroutine to finish, so no pending entries are lost at shutdown. Safe to
// call multiple times (subsequent calls are no-ops) and safe to race against
// concurrent appendRetentionEvent calls (retentionEventMu makes the close
// and the closed-check atomic with respect to each other). Terminal: after
// this call, appendRetentionEvent silently drops all events.
func flushRetentionEvents() {
	startRetentionEventWriter()
	retentionEventMu.Lock()
	if !retentionEventClosed {
		retentionEventClosed = true
		close(retentionEventCh)
	}
	retentionEventMu.Unlock()
	<-retentionEventDone
}
