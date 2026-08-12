package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

type bandwidthReport struct {
	NodeID    string        `json:"node_id"`
	Host      string        `json:"host"`
	Version   string        `json:"version"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	Proxies   []proxyReport `json:"proxies"`
	System    systemMetrics `json:"sys"`
}

type proxyReport struct {
	ID                string `json:"id"`
	Address           string `json:"addr"`
	Status            string `json:"status"`
	TotalRX           uint64 `json:"rx"`
	TotalTX           uint64 `json:"tx"`
	BillRX            uint64 `json:"bill_rx"`
	BillTX            uint64 `json:"bill_tx"`
	Clients           int64  `json:"clients"`
	MaxAge            int64  `json:"max_age_s"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`

	// A-F grade surfaced from the provider's grading pipeline (roadmap
	// step 3). Only present when the proxy has been graded; omitempty keeps
	// ungraded proxies (and older hubs) on the exact legacy payload shape.
	Score      float64  `json:"score,omitempty"`
	Graded     bool     `json:"graded,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	LastGraded int64    `json:"last_graded,omitempty"` // unix ts, 0 = never
}

type systemMetrics struct {
	HeapMiB     uint64  `json:"heap_mib"`
	SysMiB      uint64  `json:"sys_mib"`
	Connections int64   `json:"conns"`
	Pressure    float64 `json:"pressure,omitempty"`
}

// proxyStatus is the compact per-proxy fields a heartbeat carries — status
// and contract counters only, no byte-level detail. json tags must match
// hub/main.go's proxyStatus.
type proxyStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`
}

// heartbeatReport is the lightweight, high-frequency (10-30s) counterpart to
// bandwidthReport: no byte-level detail, just enough for the hub to keep
// "last seen", the Mbps rate, and per-proxy status/contracts live between
// the much less frequent full /api/report ticks (5-15m default). Its json
// tags must stay in sync with hub/main.go's heartbeatReport. Proxies is
// sparse by the time it's marshaled — see filterChangedProxies, applied by
// runHeartbeatReporter before sending.
type heartbeatReport struct {
	NodeID    string        `json:"node_id"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	TotalRX   uint64        `json:"rx"`
	TotalTX   uint64        `json:"tx"`
	Clients   int64         `json:"clients"`
	System    systemMetrics `json:"sys"`
	Proxies   []proxyStatus `json:"proxies,omitempty"`
}

// reportURLOverridePath returns ~/.urnetwork/report_url, a file an operator
// can write at any time to set, change, or disable (write "off") the hub
// target without restarting the provider. It takes precedence over
// URNETWORK_REPORT_URL, which is read once at process start and otherwise
// can't be changed without a restart.
func reportURLOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "report_url"), nil
}

// reportURLOverrideOff is the literal override-file content that force-
// disables reporting, distinct from a blank file (which is a no-op that
// falls back to envFallback — see resolveReportURL). This lets urnet-tools
// hub off turn reporting off for an already-running process without a
// restart, the same way hub set/link turn it on.
const reportURLOverrideOff = "off"

// resolveReportURL re-reads the override file on every call so a change
// takes effect on the reporter's next tick. envFallback is the value
// captured from URNETWORK_REPORT_URL at startup, used when no override file
// exists or it's empty.
func resolveReportURL(envFallback string) string {
	path, err := reportURLOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v == reportURLOverrideOff {
				return ""
			} else if v != "" {
				return v
			}
		}
	}
	return envFallback
}

// nodeNameOverridePath returns ~/.urnetwork/node_name, a file an operator can
// write at any time to change the node identity reported to the hub without
// restarting. An empty file or missing file falls back to the startup hostname.
func nodeNameOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "node_name"), nil
}

// resolveNodeName re-reads the override file on every call so a change takes
// effect on the reporter's next tick. startupName is the hostname captured at
// startup, used when no override file exists or it's empty.
func resolveNodeName(startupName string) string {
	path, err := nodeNameOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return startupName
}

// reportIntervalOverridePath returns ~/.urnetwork/report_interval, a file an
// operator can write at any time to change the hub report cadence without
// restarting the provider. It takes precedence over URNETWORK_REPORT_INTERVAL,
// which is read once at process start.
func reportIntervalOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "report_interval"), nil
}

// resolveReportInterval re-reads the override file on every call so a change
// takes effect on the reporter's next tick. startupInterval is the value
// captured from URNETWORK_REPORT_INTERVAL at startup, used when no override
// file exists or it's empty. A zero duration (or unparseable content) falls
// back to startupInterval.
func resolveReportInterval(startupInterval time.Duration) time.Duration {
	path, err := reportIntervalOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d >= 10*time.Second {
					return d
				}
			}
		}
	}
	return startupInterval
}

// alertWebhookOverridePath returns ~/.urnetwork/alert_webhook, the outage
// watcher's equivalent of reportURLOverridePath: a file an operator can
// write to set, change, or clear URNETWORK_ALERT_WEBHOOK without restarting
// the provider.
func alertWebhookOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "alert_webhook"), nil
}

// resolveAlertWebhook mirrors resolveReportURL: the override file takes
// precedence over envFallback (URNETWORK_ALERT_WEBHOOK captured at startup)
// when present and non-blank.
func resolveAlertWebhook(envFallback string) string {
	path, err := alertWebhookOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return envFallback
}

// runBandwidthReporter periodically POSTs this node's per-proxy bandwidth and
// system metrics to the fleet hub. The target is re-resolved every tick via
// resolveReportURL, so writing a URL (or "off") to ~/.urnetwork/report_url
// turns reporting on, off, or repoints it at a different hub without a
// restart; envReportURL is only the startup-time fallback used when that
// file doesn't exist or is blank. It is a best-effort telemetry loop: failures are logged but never
// retried beyond the next tick. The cadence defaults to 5m and is
// overridable via URNETWORK_REPORT_INTERVAL (min 10s). The 5m default keeps
// the hub's historical SQLite write volume modest across a large fleet; set a
// shorter interval where a more live dashboard matters. The bandwidthReport /
// proxyReport JSON shape mirrors what the hub decodes, so keep the json tags
// here in sync with hub/main.go.
func runBandwidthReporter(ctx context.Context, nodeID, host, envReportURL string, startTime time.Time) {
	hubToken := os.Getenv("URNETWORK_HUB_TOKEN")

	interval := 5 * time.Minute
	if s := os.Getenv("URNETWORK_REPORT_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 10*time.Second {
			interval = d
		}
	}
	_ = interval // keep the variable for resolveReportInterval; client created per tick below

	// startup jitter so a fleet that restarts together doesn't post on the same
	// wall-clock boundary and thundering-herd the hub. mirrors the proxy
	// benchmark probes' jittered start.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(rand.Int63n(int64(interval)))):
	}

	ticker := time.NewTicker(interval)
	// A plain `defer ticker.Stop()` binds the ticker *value* at defer time;
	// once the loop below reassigns `ticker` to a new one on an interval
	// change, that defer only ever stops the original, now-discarded
	// ticker, leaking the live one. Deferring a closure reads the current
	// value of `ticker` when the function actually returns.
	defer func() { ticker.Stop() }()

	var client *http.Client
	activeReportURL := ""
	activeInterval := interval
	var activeCACert []byte // nil = not yet read, empty = no CA cert
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Re-check the interval on every tick so a write to
		// ~/.urnetwork/report_interval takes effect without restart.
		if newInterval := resolveReportInterval(interval); newInterval != activeInterval {
			ticker.Stop()
			ticker = time.NewTicker(newInterval)
			activeInterval = newInterval
			tlog("[report] report interval changed to %s (node=%s)\n", newInterval, nodeID)
		}

		// Re-check the node name on every tick so ~/.urnetwork/node_name
		// takes effect without restart.
		activeHost := resolveNodeName(host)

		reportURL := resolveReportURL(envReportURL)
		caCert, _ := loadHubCACert()
		if reportURL != activeReportURL || !bytes.Equal(caCert, activeCACert) {
			activeCACert = caCert
			client = newClientForURL(reportURL)
			activeReportURL = reportURL
			if reportURL == "" {
				tlog("[report] hub reporting disabled (node=%s)\n", nodeID)
			} else if apiURL, err := url.JoinPath(reportURL, "/api/report"); err == nil {
				tlog("[report] posting bandwidth to %s every %s (node=%s)\n", apiURL, activeInterval, nodeID)
			}
		}
		if reportURL == "" {
			continue
		}

		apiURL, err := url.JoinPath(reportURL, "/api/report")
		if err != nil {
			tlog("[report] invalid report URL %s: %v\n", reportURL, err)
			continue
		}

		report := buildReport(nodeID, activeHost, startTime)
		if len(report.Proxies) == 0 {
			tlog("[report] skipping (0 proxies in bandwidth map)\n")
			continue
		}
		tlog("[report] sending %d proxies to %s\n", len(report.Proxies), apiURL)

		body, err := json.Marshal(report)
		if err != nil {
			tlog("[report] marshal error: %v\n", err)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
		if err != nil {
			tlog("[report] request build error: %v\n", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if hubToken != "" {
			req.Header.Set("Authorization", "Bearer "+hubToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			tlog("[report] post failed: %v\n", err)
			continue
		}
		// surface a rejecting hub instead of silently treating any response as
		// success. without this a 401/404/5xx looks identical to a 200 and the
		// fleet dashboard goes stale with no signal on the provider side. the
		// report cadence already rate-limits this, so log every occurrence.
		if resp.StatusCode/100 != 2 {
			tlog("[report] hub rejected report: %s\n", resp.Status)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func buildReport(nodeID, host string, startTime time.Time) bandwidthReport {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
	}
	metrics.Read(samples)
	heapMiB := metricBytesToMiB("/memory/classes/heap/objects:bytes", samples[0].Value)
	sysMiB := metricBytesToMiB("/memory/classes/total:bytes", samples[1].Value)

	_, dead, degraded, bandwidth, connecting := connect.ProxyHealthSnapshot()

	deadSet := make(map[string]bool, len(dead))
	for _, d := range dead {
		deadSet[d] = true
	}
	degradedSet := make(map[string]bool, len(degraded))
	for _, d := range degraded {
		degradedSet[d] = true
	}
	connectingSet := make(map[string]bool, len(connecting))
	for _, c := range connecting {
		connectingSet[c] = true
	}

	// Grade stores are read once per report, not per proxy: both files are
	// small (hundreds of entries) and the report loop covers every proxy
	// the box serves. A read failure (transient IO, first boot before the
	// stores exist) simply means no grades this round — the report itself
	// must not fail because grading data is unavailable.
	paidState, _ := readProxyState()
	urlState, _ := readProxyURLState()

	proxies := make([]proxyReport, 0, len(bandwidth))
	for key, bw := range bandwidth {
		_, ip := parseProxyString(key)

		status := "up"
		if deadSet[key] {
			status = "dead"
		} else if degradedSet[key] {
			status = "degraded"
		} else if connectingSet[key] {
			status = "connecting"
		}

		var cAcquired, cDenied int64
		if idx := parseProxyIndex(key); idx >= 0 {
			if m := globalContractMetrics.get(idx); m != nil {
				cAcquired, cDenied = m.snapshot()
			}
		}

		pr := proxyReport{
			ID:                key,
			Address:           ip,
			Status:            status,
			TotalRX:           bw.TotalRx.Load(),
			TotalTX:           bw.TotalTx.Load(),
			BillRX:            bw.BillableRx.Load(),
			BillTX:            bw.BillableTx.Load(),
			Clients:           bw.Clients.Load(),
			MaxAge:            int64(bw.MaxAge().Seconds()),
			ContractsAcquired: cAcquired,
			ContractsDenied:   cDenied,
		}
		if g, ok := proxyGradeFor(ip, paidState, urlState); ok {
			pr.Score = g.Score
			pr.Graded = true
			pr.Failed = g.Failed
			pr.Tier = g.Tier
			pr.LastGraded = g.LastGraded
		}
		proxies = append(proxies, pr)
	}

	return bandwidthReport{
		NodeID:    nodeID,
		Host:      host,
		Version:   RequireVersion(),
		Timestamp: time.Now().UTC(),
		Uptime:    time.Since(startTime).Seconds(),
		Proxies:   proxies,
		System: systemMetrics{
			HeapMiB:     heapMiB,
			SysMiB:      sysMiB,
			Connections: connect.ActiveConnectionCount(),
			Pressure:    currentPressure(),
		},
	}
}

// maxHeartbeatBackoff caps how far consecutive-failure backoff can stretch
// the heartbeat interval. A fleet-wide hub outage on a flaky link (e.g.
// Detroit) should quiet down to at most one attempt every 5m per node
// rather than retrying every base interval indefinitely.
const maxHeartbeatBackoff = 5 * time.Minute

// nextHeartbeatInterval doubles the wait for each consecutive heartbeat
// failure (base, 2x, 4x, 8x, ...), capped at maxHeartbeatBackoff, so a
// fleet doesn't retry-storm a hub that's down or unreachable. Resets to
// base as soon as a heartbeat succeeds (consecutiveFailures back to 0).
func nextHeartbeatInterval(base time.Duration, consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return base
	}
	d := base
	for i := 0; i < consecutiveFailures; i++ {
		if d >= maxHeartbeatBackoff {
			return maxHeartbeatBackoff
		}
		d *= 2
	}
	if d > maxHeartbeatBackoff {
		return maxHeartbeatBackoff
	}
	return d
}

// buildHeartbeat is buildReport's lightweight counterpart: it reuses the
// same proxy-bandwidth aggregation (buildReport already talks to the global
// bandwidth map, proxy health snapshot, and contract metrics) but projects
// down to just the top-level counters the dashboard's Mbps/last-seen display
// needs, since a heartbeat carries no per-proxy detail.
func buildHeartbeat(nodeID, host string, startTime time.Time) heartbeatReport {
	report := buildReport(nodeID, host, startTime)

	var totalRX, totalTX uint64
	var clients int64
	proxies := make([]proxyStatus, 0, len(report.Proxies))
	for _, p := range report.Proxies {
		totalRX += p.TotalRX
		totalTX += p.TotalTX
		clients += p.Clients
		proxies = append(proxies, proxyStatus{
			ID:                p.ID,
			Status:            p.Status,
			ContractsAcquired: p.ContractsAcquired,
			ContractsDenied:   p.ContractsDenied,
		})
	}

	return heartbeatReport{
		NodeID:  nodeID,
		Uptime:  report.Uptime,
		TotalRX: totalRX,
		TotalTX: totalTX,
		Clients: clients,
		System:  report.System,
		Proxies: proxies,
	}
}

// filterChangedProxies returns only the entries in current whose Status or
// contract counters differ from prev (or that have no entry in prev), plus
// the updated snapshot to pass as prev on the next call. proxyStatus is a
// plain comparable struct (string/string/int64/int64 fields), so equality
// is a simple !=. Most proxies in a fleet are idle at any given tick, so
// this keeps the heartbeat's per-proxy payload proportional to what's
// actually changing rather than to total proxy count.
func filterChangedProxies(prev map[string]proxyStatus, current []proxyStatus) ([]proxyStatus, map[string]proxyStatus) {
	next := make(map[string]proxyStatus, len(current))
	var changed []proxyStatus
	for _, p := range current {
		next[p.ID] = p
		if old, ok := prev[p.ID]; !ok || old != p {
			changed = append(changed, p)
		}
	}
	return changed, next
}

// postHeartbeat marshals and POSTs a heartbeat to the hub. Returns true on 2xx.
func postHeartbeat(ctx context.Context, client *http.Client, apiURL, hubToken string, hb heartbeatReport) bool {
	body, err := json.Marshal(hb)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if hubToken != "" {
		req.Header.Set("Authorization", "Bearer "+hubToken)
	}
	resp, err := client.Do(req)
	ok := err == nil && resp.StatusCode/100 == 2
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return ok
}

// runHeartbeatReporter periodically POSTs a lightweight liveness/rate ping
// to the hub's /api/heartbeat, on a much shorter cadence than
// runBandwidthReporter's full /api/report (default 15s vs 5m). It shares
// resolveReportURL/resolveNodeName with the full reporter so hub target and
// node name changes apply to both without a restart. The hub only accepts a
// heartbeat for a node it already knows about (established by a prior full
// report), so an all-heartbeats-rejected hub log is expected right after a
// provider restart until the first /api/report lands.
func runHeartbeatReporter(ctx context.Context, nodeID, host, envReportURL string, startTime time.Time) {
	hubToken := os.Getenv("URNETWORK_HUB_TOKEN")

	baseInterval := 15 * time.Second
	if s := os.Getenv("URNETWORK_HEARTBEAT_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 5*time.Second {
			baseInterval = d
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(rand.Int63n(int64(baseInterval)))):
	}

	ticker := time.NewTicker(baseInterval)
	// See the comment on the equivalent defer above: a plain
	// `defer ticker.Stop()` would only ever stop this original ticker, not
	// any reassigned below on an interval change.
	defer func() { ticker.Stop() }()

	// The client is cached across ticks and only rebuilt when the target
	// hub URL changes, so a 15s heartbeat cadence doesn't pay a fresh
	// TCP+TLS handshake every tick the way a client-per-request would — at
	// fleet scale (dozens of nodes) that handshake cost is what actually
	// stresses a hub on a flaky link, not the ~200-byte JSON payload.
	var client *http.Client
	var activeReportURL string
	consecutiveFailures := 0
	activeInterval := baseInterval
	prevProxyStatus := map[string]proxyStatus{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		reportURL := resolveReportURL(envReportURL)
		if reportURL == "" {
			continue
		}
		if reportURL != activeReportURL {
			client = newClientForURL(reportURL)
			activeReportURL = reportURL
		}

		apiURL, err := url.JoinPath(reportURL, "/api/heartbeat")
		if err != nil {
			continue
		}

		activeHost := resolveNodeName(host)
		hb := buildHeartbeat(nodeID, activeHost, startTime)
		changedProxies, nextProxyStatus := filterChangedProxies(prevProxyStatus, hb.Proxies)

		const maxHeartbeatProxies = 200
		if len(changedProxies) <= maxHeartbeatProxies {
			hb.Proxies = changedProxies
			allOK := postHeartbeat(ctx, client, apiURL, hubToken, hb)
			if allOK {
				prevProxyStatus = nextProxyStatus
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
			}
		} else {
			allOK := true
			for i := 0; i < len(changedProxies); i += maxHeartbeatProxies {
				end := i + maxHeartbeatProxies
				if end > len(changedProxies) {
					end = len(changedProxies)
				}
				batch := changedProxies[i:end]
				hb.Proxies = batch
				if ok := postHeartbeat(ctx, client, apiURL, hubToken, hb); ok {
					for _, p := range batch {
						prevProxyStatus[p.ID] = nextProxyStatus[p.ID]
					}
				} else {
					allOK = false
					break
				}
			}
			if allOK {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
			}
		}

		// A flaky link to the hub (e.g. an outage) shouldn't have every
		// node in the fleet retry-storming it every base interval — back
		// off the next tick's wait on consecutive failures, capped, and
		// snap straight back to baseInterval the moment it recovers.
		if newInterval := nextHeartbeatInterval(baseInterval, consecutiveFailures); newInterval != activeInterval {
			ticker.Stop()
			ticker = time.NewTicker(newInterval)
			activeInterval = newInterval
		}
	}
}

func parseProxyIndex(key string) int {
	idx := strings.IndexByte(key, ']')
	if idx <= 0 {
		return -1
	}
	start := strings.IndexByte(key[:idx], '[')
	if start < 0 {
		return -1
	}
	n, err := strconv.Atoi(key[start+1 : idx])
	if err != nil {
		return -1
	}
	return n
}

func hubPinPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "hub.pin"), nil
}

func loadPinnedFPs() map[string]bool {
	pins := map[string]bool{}
	path, err := hubPinPath()
	if err != nil {
		return pins
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pins
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			pins[line] = true
		}
	}
	return pins
}

var loggedLegacyPinDeprecation atomic.Bool

func newClientForURL(reportURL string) *http.Client {
	if !strings.HasPrefix(reportURL, "https://") {
		return &http.Client{Timeout: 10 * time.Second}
	}

	// CA-based verification
	if pool, ok := loadHubCAPool(); ok {
		serverName := ""
		if u, err := url.Parse(reportURL); err == nil {
			if host := u.Hostname(); host != "" && net.ParseIP(host) == nil {
				serverName = host
			}
		}
		return &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: true, // we verify via VerifyConnection
					VerifyConnection:   verifyHubChain(pool, serverName),
				},
			},
		}
	}

	// Legacy fingerprint pinning (deprecated)
	pins := loadPinnedFPs()
	if len(pins) > 0 {
		if loggedLegacyPinDeprecation.CompareAndSwap(false, true) {
			tlog("[hub] hub.pin is deprecated — re-run 'urnet-tools hub link' to switch to CA-based trust\n")
		}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("no peer certificates")
			}
			fp := fmt.Sprintf("SHA256:%x", sha256.Sum256(cs.PeerCertificates[0].Raw))
			if pins[fp] {
				return nil
			}
			return fmt.Errorf("certificate fingerprint %s is not pinned", fp)
		}
		return &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
		}
	}

	// Try system CA pool (works with Cloudflare Tunnel, Caddy+LE, etc.)
	if pool, err := x509.SystemCertPool(); err == nil {
		return &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
					RootCAs:    pool,
				},
			},
		}
	}

	// Fail closed - no trust material
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				VerifyConnection: func(cs tls.ConnectionState) error {
					return fmt.Errorf("no hub CA installed — run 'urnet-tools hub link https://hub:port' or hub onboarding before using HTTPS")
				},
			},
		},
	}
}

func hubCACertPath() (string, error) {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".urnetwork", "hub_ca.pem"), nil
}

func loadHubCACert() ([]byte, bool) {
	path, err := hubCACertPath()
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func loadHubCAPool() (*x509.CertPool, bool) {
	data, ok := loadHubCACert()
	if !ok {
		return nil, false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, false
	}
	return pool, true
}

// bootstrapHubCA fetches the hub's CA certificate at provider startup using
// the hub token, so Docker users can deploy containers already authenticated
// to their hub without running urnet-tools hub link manually.
//
// It tries a certificate-verified fetch first (system trust store) — this is
// the fully safe path and covers hubs behind Cloudflare Tunnel or Caddy+LE
// with a publicly-trusted cert; the hub token is never sent over an
// unverified connection there. Only if that fails (the common case for a
// direct password-derived CA hub, whose leaf isn't in any public trust store
// yet) does it fall back to an unverified fetch, which is a TOFU bootstrap:
// the token is sent before the hub's identity can be confirmed, and a
// network attacker present at that exact moment could read it or hand back a
// forged CA cert. That fallback is intentionally kept (it's what makes
// zero-touch Docker bootstrap possible at all) but is logged loudly so it's
// never a silent tradeoff — operators who can't accept it on their network
// should run 'urnet-tools hub link' manually instead, which requires an
// explicit onboard token rather than the standing fleet-wide hub token.
//
// Once the CA cert is written to hub_ca.pem, newClientForURL picks it up for
// proper chain verification on all subsequent requests.
func bootstrapHubCA(ctx context.Context, reportURL, hubToken string) {
	if reportURL == "" || hubToken == "" {
		return
	}
	if !strings.HasPrefix(reportURL, "https://") {
		return
	}

	caPath, err := hubCACertPath()
	if err != nil {
		tlog("[hub] CA cert path error: %v\n", err)
		return
	}

	if _, err := os.Stat(caPath); err == nil {
		return
	}

	apiURL, err := url.JoinPath(reportURL, "/api/ca-cert")
	if err != nil {
		tlog("[hub] bootstrap URL error: %v\n", err)
		return
	}

	tlog("[hub] bootstrapping CA cert from %s\n", apiURL)

	caPEM, ok := fetchHubCACert(ctx, apiURL, hubToken, &http.Client{Timeout: 10 * time.Second})
	if !ok {
		tlog("[hub] WARNING: verified CA cert fetch failed (expected for a direct password-derived CA hub) — falling back to an unverified fetch. The hub token will be sent before the hub's identity is confirmed. Only safe if hub and provider share a trusted network at boot; run 'urnet-tools hub link %s' manually instead if you can't accept that.\n", reportURL)
		insecureClient := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		caPEM, ok = fetchHubCACert(ctx, apiURL, hubToken, insecureClient)
		if !ok {
			return
		}
	}

	if err := writeHubCACertAtomic(caPath, caPEM); err != nil {
		tlog("[hub] bootstrap CA cert write error: %v\n", err)
		return
	}

	tlog("[hub] CA cert installed from hub token bootstrap\n")
}

// fetchHubCACert performs the actual GET against /api/ca-cert and extracts
// ca_pem from the response. Errors are logged by the caller (the verified
// attempt fails silently into the caller's fallback branch; the caller logs
// once there instead of twice).
func fetchHubCACert(ctx context.Context, apiURL, hubToken string, client *http.Client) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		tlog("[hub] bootstrap request error: %v\n", err)
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+hubToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		tlog("[hub] bootstrap CA cert rejected: %s — %s\n", resp.Status, strings.TrimSpace(string(body)))
		return "", false
	}

	var result struct {
		CAPEM string `json:"ca_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		tlog("[hub] bootstrap CA cert parse error: %v\n", err)
		return "", false
	}

	if result.CAPEM == "" {
		tlog("[hub] bootstrap CA cert response missing ca_pem\n")
		return "", false
	}

	return result.CAPEM, true
}

// writeHubCACertAtomic writes the CA cert via temp-file-then-rename so a
// crash or power loss mid-write can never leave a truncated hub_ca.pem on
// disk — the bootstrap's "skip if the file already exists" check in
// bootstrapHubCA depends on that file only ever existing in a complete state.
func writeHubCACertAtomic(caPath, caPEM string) error {
	dir := filepath.Dir(caPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".hub_ca.pem.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.WriteString(caPEM + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, caPath)
}

func verifyHubChain(pool *x509.CertPool, serverName string) func(cs tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("no peer certificates")
		}

		intermediates := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			intermediates.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
		}

		opts := x509.VerifyOptions{
			Roots:         pool,
			Intermediates: intermediates,
			DNSName:       serverName,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
			return fmt.Errorf("hub CA verification failed (%w) — the hub may have been redeployed with a different password; re-run 'urnet-tools hub link' or hub onboarding", err)
		}
		return nil
	}
}
