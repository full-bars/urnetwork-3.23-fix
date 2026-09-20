package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// --- Operational counters (in-process, exposed via /metrics) ---

var (
	controlCmdsAcked atomic.Int64
	controlCmdsSet   atomic.Int64
	controlCmdsGet   atomic.Int64
	controlCmdsClear atomic.Int64
)

// IncrControlCmd increments the appropriate counter based on the command type.
func IncrControlCmd(cmd string) {
	controlCmdsAcked.Add(1)
	switch cmd {
	case "set":
		controlCmdsSet.Add(1)
	case "get":
		controlCmdsGet.Add(1)
	case "clear":
		controlCmdsClear.Add(1)
	}
}

// --- Startup diagnostics (crash/restart/upgrade detection) ---

var providerStartTime = time.Now()

type startupDiagnostics struct {
	mu               sync.Mutex
	loaded           bool
	previousVersion  string
	cleanShutdown    bool   // was the previous shutdown clean?
	restartReason    string // why this process started; see classifyRestart
	previousVersion_ string // stored on disk
}

var startupDiag = &startupDiagnostics{}

// markCleanShutdown writes a marker file so next startup knows this was clean.
func markCleanShutdown() {
	stateDir := mustStateDir()
	if stateDir == "" {
		return
	}
	path := filepath.Join(stateDir, ".clean-shutdown")
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0600); err != nil {
		tlog("[metrics] failed to write shutdown marker: %v\n", err)
	}
}

// detectStartup reads the shutdown marker and version file, then removes the
// shutdown marker (next clean exit will re-create it).
func detectStartup() {
	startupDiag.mu.Lock()
	defer startupDiag.mu.Unlock()
	if startupDiag.loaded {
		return
	}
	startupDiag.loaded = true

	stateDir := mustStateDir()
	if stateDir == "" {
		return
	}

	// Check for clean-shutdown marker
	shutdownPath := filepath.Join(stateDir, ".clean-shutdown")
	if data, err := os.ReadFile(shutdownPath); err == nil {
		startupDiag.cleanShutdown = true
		_ = data // timestamp available if needed
	}
	// Remove marker — if we crash, it won't be re-created
	os.Remove(shutdownPath)

	// Check for version change (upgrade detection)
	versionPath := filepath.Join(stateDir, ".provider_version")
	if data, err := os.ReadFile(versionPath); err == nil {
		startupDiag.previousVersion = strings.TrimSpace(string(data))
	}
	// Write current version
	os.WriteFile(versionPath, []byte(RequireVersion()), 0600)

	// The restart marker is consumed the same way: read, then deleted, so it
	// only ever describes the restart that just happened.
	marker := consumeRestartMarker(stateDir, time.Now())
	startupDiag.restartReason = classifyRestart(marker, startupDiag.cleanShutdown, startupDiag.previousVersion)
}

// mustStateDir returns ~/.urnetwork or "" on error.
func mustStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".urnetwork")
}

// --- Persistent error counts ---

type persistentErrors struct {
	mu     sync.Mutex
	counts map[string]uint64
	path   string
	dirty  uint64 // new errors since last flush
}

var persistentErrStore *persistentErrors

// errorFlushTrigger signals the periodic flush goroutine when many errors
// have accumulated. Buffered so IncrPersistentError never blocks.
var errorFlushTrigger = make(chan uint64, 1)

func initPersistentErrors() {
	stateDir := mustStateDir()
	if stateDir == "" {
		return
	}
	path := filepath.Join(stateDir, "error_counts.json")
	pe := &persistentErrors{counts: map[string]uint64{}, path: path}

	// Load with checksum recovery (primary → .bak → quarantine)
	var loaded map[string]uint64
	if ok, err := loadJSONWithRecovery(path, &loaded); ok {
		pe.counts = loaded
	} else if err != nil {
		tlog("[metrics] error loading error counts: %v\n", err)
	}

	persistentErrStore = pe
	go pe.periodicFlush()
}

// IncrPersistentError increments a category's persistent error count.
func IncrPersistentError(cat string) {
	if persistentErrStore == nil {
		return
	}
	persistentErrStore.mu.Lock()
	persistentErrStore.counts[cat]++
	persistentErrStore.dirty++
	dirty := persistentErrStore.dirty
	persistentErrStore.mu.Unlock()

	// Signal flush goroutine if threshold reached (non-blocking)
	if dirty > 100 {
		select {
		case errorFlushTrigger <- dirty:
		default:
		}
	}
}

// periodicFlush writes error_counts.json every 5 minutes or when 100+
// new errors have accumulated, using atomic writes for crash safety.
func (pe *persistentErrors) periodicFlush() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			pe.flushAtomic()
		case <-errorFlushTrigger:
			pe.flushAtomic()
		}
	}
}

// flushAtomic writes error counts using atomicWriteJSON. Resets dirty counter.
func (pe *persistentErrors) flushAtomic() {
	pe.mu.Lock()
	if pe.dirty == 0 {
		pe.mu.Unlock()
		return
	}
	snapshot := make(map[string]uint64, len(pe.counts))
	for k, v := range pe.counts {
		snapshot[k] = v
	}
	pe.dirty = 0
	pe.mu.Unlock()

	if err := atomicWriteJSON(pe.path, snapshot); err != nil {
		tlog("[metrics] periodic error flush failed: %v\n", err)
	}
}

// FlushPersistentErrors writes error counts to disk atomically (call on shutdown).
func FlushPersistentErrors() {
	if persistentErrStore == nil {
		return
	}
	persistentErrStore.mu.Lock()
	snapshot := make(map[string]uint64, len(persistentErrStore.counts))
	for k, v := range persistentErrStore.counts {
		snapshot[k] = v
	}
	persistentErrStore.dirty = 0
	persistentErrStore.mu.Unlock()

	if err := atomicWriteJSON(persistentErrStore.path, snapshot); err != nil {
		tlog("[metrics] shutdown error flush failed: %v\n", err)
	}
}

// snapshotErrors returns a copy of the persistent error counts.
func snapshotErrors() map[string]uint64 {
	if persistentErrStore == nil {
		return nil
	}
	persistentErrStore.mu.Lock()
	defer persistentErrStore.mu.Unlock()
	out := make(map[string]uint64, len(persistentErrStore.counts))
	for k, v := range persistentErrStore.counts {
		out[k] = v
	}
	return out
}

// readHotswapDeclinesFromDisk reads the hotswap decline/success counters
// that the urnet-tools CLI writes to <stateDir>/.hotswap_declines.json.
// Returns nil when the file is absent or unreadable (normal: no updates
// have been attempted on this provider yet).
func readHotswapDeclinesFromDisk() map[string]int64 {
	stateDir := mustStateDir()
	if stateDir == "" {
		return nil
	}
	path := filepath.Join(stateDir, ".hotswap_declines.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var dc struct {
		Counts map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(data, &dc); err != nil || dc.Counts == nil {
		return nil
	}
	return dc.Counts
}

// providerExtraMetrics generates the Prometheus text-format lines that only
// the provider can supply. Called by connect.PrometheusHandler on every scrape.
func providerExtraMetrics() string {
	detectStartup()
	var b strings.Builder

	// --- Provider info (version) ---
	// A gauge fixed at 1 that carries the version as a label. The classic
	// text format this handler serves has no "info" type, and Prometheus
	// rejects the entire scrape on one. Uptime and the proxy count are their
	// own metrics (urnet_uptime_seconds, urnet_proxy_pool_size); as labels
	// they changed on every scrape and created a new series each time.
	proxyVersion := RequireVersion()
	fmt.Fprintf(&b, "# HELP urnet_info Provider identity: the version label, value always 1.\n")
	fmt.Fprintf(&b, "# TYPE urnet_info gauge\n")
	fmt.Fprintf(&b, "urnet_info{version=%s} 1\n", connect.PrometheusLabelValue(proxyVersion))

	// --- Startup diagnostics (crash/restart/upgrade) ---
	startupDiag.mu.Lock()
	wasClean := startupDiag.cleanShutdown
	prevVersion := startupDiag.previousVersion
	startupDiag.mu.Unlock()
	wasCleanStr := "0"
	if wasClean {
		wasCleanStr = "1"
	}
	restarted := "0"
	if !wasClean && prevVersion != "" {
		restarted = "1" // had a previous run that didn't shut down cleanly
	}
	upgraded := "0"
	if prevVersion != "" && prevVersion != proxyVersion {
		upgraded = "1"
	}
	fmt.Fprintf(&b, "# HELP urnet_startup_clean_shutdown Whether the previous shutdown was clean.\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_clean_shutdown gauge\n")
	fmt.Fprintf(&b, "urnet_startup_clean_shutdown %s\n", wasCleanStr)

	fmt.Fprintf(&b, "# HELP urnet_startup_restarted Whether the provider restarted (dirty or clean).\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_restarted gauge\n")
	fmt.Fprintf(&b, "urnet_startup_restarted %s\n", restarted)

	fmt.Fprintf(&b, "# HELP urnet_startup_upgraded Whether the version changed since last run.\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_upgraded gauge\n")
	fmt.Fprintf(&b, "urnet_startup_upgraded %s\n", upgraded)

	if prevVersion != "" {
		fmt.Fprintf(&b, "# HELP urnet_startup_previous_version Version before last upgrade.\n")
		fmt.Fprintf(&b, "# TYPE urnet_startup_previous_version gauge\n")
		fmt.Fprintf(&b, "urnet_startup_previous_version{version=%s} 1\n", connect.PrometheusLabelValue(prevVersion))
	}

	// --- Control socket commands ---
	fmt.Fprintf(&b, "# HELP urnet_control_commands_total Lifetime control socket commands acked.\n")
	fmt.Fprintf(&b, "# TYPE urnet_control_commands_total counter\n")
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"get\"} %d\n", controlCmdsGet.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"set\"} %d\n", controlCmdsSet.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"clear\"} %d\n", controlCmdsClear.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"any\"} %d\n", controlCmdsAcked.Load())

	// --- Lifetime persisted metrics ---
	if lm := lifetimeStore; lm != nil {
		pqe, clas, up, deny, recov, lost, bill := lm.Snapshot()
		fmt.Fprintf(&b, "# HELP urnet_lifetime_sessions_total Lifetime sessions opened (persists across restarts).\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_sessions_total counter\n")
		fmt.Fprintf(&b, "urnet_lifetime_sessions_total{encryption=\"pqe\"} %d\n", pqe)
		fmt.Fprintf(&b, "urnet_lifetime_sessions_total{encryption=\"classical\"} %d\n", clas)

		fmt.Fprintf(&b, "# HELP urnet_lifetime_contracts_total Lifetime contract outcomes.\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_contracts_total counter\n")
		fmt.Fprintf(&b, "urnet_lifetime_contracts_total{result=\"acquired\"} %d\n", up)
		fmt.Fprintf(&b, "urnet_lifetime_contracts_total{result=\"denied\"} %d\n", deny)

		fmt.Fprintf(&b, "# HELP urnet_lifetime_proxies_total Lifetime proxy transitions.\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_proxies_total counter\n")
		fmt.Fprintf(&b, "urnet_lifetime_proxies_total{event=\"recovered\"} %d\n", recov)
		fmt.Fprintf(&b, "urnet_lifetime_proxies_total{event=\"lost\"} %d\n", lost)

		fmt.Fprintf(&b, "# HELP urnet_lifetime_billable_bytes_total Lifetime billable bytes transferred.\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_billable_bytes_total counter\n")
		fmt.Fprintf(&b, "urnet_lifetime_billable_bytes_total %d\n", bill)
	}

	// --- Persistent error counts (survive restarts) ---
	if errCounts := snapshotErrors(); errCounts != nil {
		fmt.Fprintf(&b, "# HELP urnet_lifetime_errors_total Persistent error counts by category (survive restarts).\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_errors_total counter\n")
		for _, cat := range slices.Sorted(maps.Keys(errCounts)) {
			fmt.Fprintf(&b, "urnet_lifetime_errors_total{category=%s} %d\n", connect.PrometheusLabelValue(string(cat)), errCounts[cat])
		}
	}

	// --- Hotswap decline/success counters (written by urnet-tools CLI) ---
	if hsCounts := readHotswapDeclinesFromDisk(); len(hsCounts) > 0 {
		fmt.Fprintf(&b, "# HELP urnet_hotswap_outcomes_total HotSwap attempt outcomes since last install.\n")
		fmt.Fprintf(&b, "# TYPE urnet_hotswap_outcomes_total counter\n")
		for _, reason := range slices.Sorted(maps.Keys(hsCounts)) {
			fmt.Fprintf(&b, "urnet_hotswap_outcomes_total{reason=%s} %d\n", connect.PrometheusLabelValue(reason), hsCounts[reason])
		}
	}

	// --- PQE session counts ---
	pq := pqeTotalCounts()
	fmt.Fprintf(&b, "# HELP urnet_sessions_pqe Active PQE sessions.\n")
	fmt.Fprintf(&b, "# TYPE urnet_sessions_pqe gauge\n")
	fmt.Fprintf(&b, "urnet_sessions_pqe %d\n", pq.ActivePQE)

	fmt.Fprintf(&b, "# HELP urnet_sessions_classical Active classical sessions.\n")
	fmt.Fprintf(&b, "# TYPE urnet_sessions_classical gauge\n")
	fmt.Fprintf(&b, "urnet_sessions_classical %d\n", pq.ActiveClas)

	// Hour/day/week are sliding windows and go down as sessions age out, so
	// they are gauges. Only the lifetime count is a counter; rate() over a
	// value that decreases reads every drop as a counter reset.
	fmt.Fprintf(&b, "# HELP urnet_sessions_opened_total Sessions opened over the provider's lifetime.\n")
	fmt.Fprintf(&b, "# TYPE urnet_sessions_opened_total counter\n")
	fmt.Fprintf(&b, "urnet_sessions_opened_total{encryption=\"pqe\"} %d\n", pq.PQELifetime)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{encryption=\"classical\"} %d\n", pq.ClasLifetime)

	fmt.Fprintf(&b, "# HELP urnet_sessions_opened_recent Sessions opened in the last hour, day, or week.\n")
	fmt.Fprintf(&b, "# TYPE urnet_sessions_opened_recent gauge\n")
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"hour\",encryption=\"pqe\"} %d\n", pq.PQEHour)
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"day\",encryption=\"pqe\"} %d\n", pq.PQEDay)
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"week\",encryption=\"pqe\"} %d\n", pq.PQEWeek)
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"hour\",encryption=\"classical\"} %d\n", pq.ClasHour)
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"day\",encryption=\"classical\"} %d\n", pq.ClasDay)
	fmt.Fprintf(&b, "urnet_sessions_opened_recent{window=\"week\",encryption=\"classical\"} %d\n", pq.ClasWeek)

	// --- Resource pressure ---
	pressure := currentPressure()
	fmt.Fprintf(&b, "# HELP urnet_pressure_score Resource pressure (0=ok, 1=emergency).\n")
	fmt.Fprintf(&b, "# TYPE urnet_pressure_score gauge\n")
	fmt.Fprintf(&b, "urnet_pressure_score %g\n", pressure)

	// --- DoH failures ---
	dohFails := connect.GetDohFailureCount()
	fmt.Fprintf(&b, "# HELP urnet_doh_failures_total DNS-over-HTTPS failures.\n")
	fmt.Fprintf(&b, "# TYPE urnet_doh_failures_total counter\n")
	fmt.Fprintf(&b, "urnet_doh_failures_total %d\n", dohFails)

	// --- Proxy grades ---
	state, stateErr := readProxyState()
	urlState, urlErr := readProxyURLState()

	if stateErr == nil {
		fmt.Fprintf(&b, "# HELP urnet_proxies_known Proxies in the provider's proxy state.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxies_known gauge\n")
		fmt.Fprintf(&b, "urnet_proxies_known %d\n", len(state.Proxies))

		gradeDist := map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "E": 0, "F": 0, "ungraded": 0}
		healthDist := map[string]int{}
		var totalAuthFailures int64
		for _, entry := range state.Proxies {
			if entry.Graded {
				tier := proxyGradeTier(entry.Score)
				gradeDist[tier]++
			} else {
				gradeDist["ungraded"]++
			}
			healthDist[entry.Health]++
			totalAuthFailures += entry.AuthFailures
		}
		fmt.Fprintf(&b, "# HELP urnet_proxy_grades Count by grade tier.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_grades gauge\n")
		for _, tier := range slices.Sorted(maps.Keys(gradeDist)) {
			fmt.Fprintf(&b, "urnet_proxy_grades{tier=%s} %d\n", connect.PrometheusLabelValue(tier), gradeDist[tier])
		}

		fmt.Fprintf(&b, "# HELP urnet_proxy_health Count by health status.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_health gauge\n")
		for _, health := range slices.Sorted(maps.Keys(healthDist)) {
			fmt.Fprintf(&b, "urnet_proxy_health{status=%s} %d\n", connect.PrometheusLabelValue(health), healthDist[health])
		}

		if !state.StartedAt.IsZero() {
			providerAge := time.Since(state.StartedAt).Seconds()
			fmt.Fprintf(&b, "# HELP urnet_provider_proxy_age_seconds How long managing current proxy set.\n")
			fmt.Fprintf(&b, "# TYPE urnet_provider_proxy_age_seconds gauge\n")
			fmt.Fprintf(&b, "urnet_provider_proxy_age_seconds %g\n", providerAge)
		}

		// Grading churn
		var recentlyGraded, staleGraded int
		cutoff := time.Now().Add(-1 * time.Hour)
		for _, entry := range state.Proxies {
			if entry.Graded && !entry.LastGraded.IsZero() {
				if entry.LastGraded.After(cutoff) {
					recentlyGraded++
				} else {
					staleGraded++
				}
			}
		}
		fmt.Fprintf(&b, "# HELP urnet_proxy_graded_recent Proxies graded in last hour.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_graded_recent gauge\n")
		fmt.Fprintf(&b, "urnet_proxy_graded_recent %d\n", recentlyGraded)

		fmt.Fprintf(&b, "# HELP urnet_proxy_graded_stale Proxies with stale grade.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_graded_stale gauge\n")
		fmt.Fprintf(&b, "urnet_proxy_graded_stale %d\n", staleGraded)

		// A sum over the proxies currently in state: removing a proxy drops
		// it, so this is a gauge, not a counter.
		fmt.Fprintf(&b, "# HELP urnet_proxy_auth_failures Auth failures summed over the proxies currently in state.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_auth_failures gauge\n")
		fmt.Fprintf(&b, "urnet_proxy_auth_failures %d\n", totalAuthFailures)
	}

	// --- URL proxy grades ---
	if urlErr == nil && urlState != nil {
		var urlUngraded int
		urlGradeDist := map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "E": 0, "F": 0}
		for _, entry := range urlState.Cache {
			if entry.Graded {
				tier := proxyGradeTier(entry.Score)
				urlGradeDist[tier]++
			} else {
				urlUngraded++
			}
		}
		fmt.Fprintf(&b, "# HELP urnet_url_proxy_grades URL proxy grade distribution.\n")
		fmt.Fprintf(&b, "# TYPE urnet_url_proxy_grades gauge\n")
		for _, tier := range slices.Sorted(maps.Keys(urlGradeDist)) {
			fmt.Fprintf(&b, "urnet_url_proxy_grades{tier=%s} %d\n", connect.PrometheusLabelValue(tier), urlGradeDist[tier])
		}
		fmt.Fprintf(&b, "# HELP urnet_url_proxy_ungraded URL proxies not yet graded.\n")
		fmt.Fprintf(&b, "# TYPE urnet_url_proxy_ungraded gauge\n")
		fmt.Fprintf(&b, "urnet_url_proxy_ungraded %d\n", urlUngraded)
	}

	return b.String()
}
