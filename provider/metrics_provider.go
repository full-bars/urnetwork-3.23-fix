package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	previousVersion_ string // stored on disk
}

var startupDiag = &startupDiagnostics{}

// markCleanShutdown writes a marker file so next startup knows this was clean.
func markCleanShutdown() {
	path := filepath.Join(mustStateDir(), ".clean-shutdown")
	os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0600)
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
}

var persistentErrStore *persistentErrors

func initPersistentErrors() {
	stateDir := mustStateDir()
	if stateDir == "" {
		return
	}
	path := filepath.Join(stateDir, "error_counts.json")
	pe := &persistentErrors{counts: map[string]uint64{}, path: path}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &pe.counts)
	}
	persistentErrStore = pe
}

// IncrPersistentError increments a category's persistent error count.
func IncrPersistentError(cat string) {
	if persistentErrStore == nil {
		return
	}
	persistentErrStore.mu.Lock()
	persistentErrStore.counts[cat]++
	persistentErrStore.mu.Unlock()
}

// FlushPersistentErrors writes the error counts to disk (call on shutdown).
func FlushPersistentErrors() {
	if persistentErrStore == nil {
		return
	}
	persistentErrStore.mu.Lock()
	defer persistentErrStore.mu.Unlock()
	data, err := json.Marshal(persistentErrStore.counts)
	if err != nil {
		return
	}
	os.WriteFile(persistentErrStore.path, data, 0600)
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

// providerExtraMetrics generates the Prometheus text-format lines that only
// the provider can supply. Called by connect.PrometheusHandler on every scrape.
func providerExtraMetrics() string {
	detectStartup()
	var b strings.Builder

	uptime := time.Since(providerStartTime).Seconds()

	// --- Provider info (version, proxy count, human-readable uptime) ---
	proxyCount := 0
	proxyVersion := RequireVersion()
	if state, err := readProxyState(); err == nil {
		proxyCount = len(state.Proxies)
	}
	fmt.Fprintf(&b, "# HELP urnet_info Provider identity and uptime (Grafana display metric).\n")
	fmt.Fprintf(&b, "# TYPE urnet_info info\n")
	fmt.Fprintf(&b, "urnet_info{version=%q,proxies=%q,uptime=%q} 1\n",
		proxyVersion,
		fmt.Sprintf("%d", proxyCount),
		connect.FormatDuration(uptime))

	// --- Startup diagnostics (crash/restart/upgrade) ---
	wasClean := "0"
	if startupDiag.cleanShutdown {
		wasClean = "1"
	}
	restarted := "0"
	if !startupDiag.cleanShutdown && startupDiag.previousVersion != "" {
		restarted = "1" // had a previous run that didn't shut down cleanly
	}
	upgraded := "0"
	if startupDiag.previousVersion != "" && startupDiag.previousVersion != proxyVersion {
		upgraded = "1"
	}
	fmt.Fprintf(&b, "# HELP urnet_startup_clean_shutdown Whether the previous shutdown was clean.\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_clean_shutdown gauge\n")
	fmt.Fprintf(&b, "urnet_startup_clean_shutdown %s\n", wasClean)

	fmt.Fprintf(&b, "# HELP urnet_startup_restarted Whether the provider restarted (dirty or clean).\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_restarted gauge\n")
	fmt.Fprintf(&b, "urnet_startup_restarted %s\n", restarted)

	fmt.Fprintf(&b, "# HELP urnet_startup_upgraded Whether the version changed since last run.\n")
	fmt.Fprintf(&b, "# TYPE urnet_startup_upgraded gauge\n")
	fmt.Fprintf(&b, "urnet_startup_upgraded %s\n", upgraded)

	if startupDiag.previousVersion != "" {
		fmt.Fprintf(&b, "# HELP urnet_startup_previous_version Version before last upgrade.\n")
		fmt.Fprintf(&b, "# TYPE urnet_startup_previous_version info\n")
		fmt.Fprintf(&b, "urnet_startup_previous_version{version=%q} 1\n", startupDiag.previousVersion)
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

		fmt.Fprintf(&b, "# HELP urnet_lifetime_billable_bytes Lifetime billable bytes transferred.\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_billable_bytes counter\n")
		fmt.Fprintf(&b, "urnet_lifetime_billable_bytes %d\n", bill)
	}

	// --- Persistent error counts (survive restarts) ---
	if errCounts := snapshotErrors(); errCounts != nil {
		fmt.Fprintf(&b, "# HELP urnet_lifetime_errors_total Persistent error counts by category (survive restarts).\n")
		fmt.Fprintf(&b, "# TYPE urnet_lifetime_errors_total counter\n")
		for cat, count := range errCounts {
			fmt.Fprintf(&b, "urnet_lifetime_errors_total{category=%q} %d\n", cat, count)
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

	fmt.Fprintf(&b, "# HELP urnet_sessions_opened_total Sessions opened in time windows.\n")
	fmt.Fprintf(&b, "# TYPE urnet_sessions_opened_total counter\n")
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"hour\",encryption=\"pqe\"} %d\n", pq.PQEHour)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"day\",encryption=\"pqe\"} %d\n", pq.PQEDay)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"week\",encryption=\"pqe\"} %d\n", pq.PQEWeek)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"lifetime\",encryption=\"pqe\"} %d\n", pq.PQELifetime)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"hour\",encryption=\"classical\"} %d\n", pq.ClasHour)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"day\",encryption=\"classical\"} %d\n", pq.ClasDay)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"week\",encryption=\"classical\"} %d\n", pq.ClasWeek)
	fmt.Fprintf(&b, "urnet_sessions_opened_total{window=\"lifetime\",encryption=\"classical\"} %d\n", pq.ClasLifetime)

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
		fmt.Fprintf(&b, "# HELP urnet_proxies_total Total proxies known.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxies_total gauge\n")
		fmt.Fprintf(&b, "urnet_proxies_total %d\n", len(state.Proxies))

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
		for tier, count := range gradeDist {
			fmt.Fprintf(&b, "urnet_proxy_grades{tier=%q} %d\n", tier, count)
		}

		fmt.Fprintf(&b, "# HELP urnet_proxy_health Count by health status.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_health gauge\n")
		for health, count := range healthDist {
			fmt.Fprintf(&b, "urnet_proxy_health{status=%q} %d\n", health, count)
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

		fmt.Fprintf(&b, "# HELP urnet_proxy_auth_failures_total Auth failures across all proxies.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_auth_failures_total counter\n")
		fmt.Fprintf(&b, "urnet_proxy_auth_failures_total %d\n", totalAuthFailures)
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
		for tier, count := range urlGradeDist {
			fmt.Fprintf(&b, "urnet_url_proxy_grades{tier=%q} %d\n", tier, count)
		}
		fmt.Fprintf(&b, "# HELP urnet_url_proxy_ungraded URL proxies not yet graded.\n")
		fmt.Fprintf(&b, "# TYPE urnet_url_proxy_ungraded gauge\n")
		fmt.Fprintf(&b, "urnet_url_proxy_ungraded %d\n", urlUngraded)
	}

	return b.String()
}
