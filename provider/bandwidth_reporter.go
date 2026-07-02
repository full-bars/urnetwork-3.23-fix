package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
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
}

type systemMetrics struct {
	HeapMiB     uint64 `json:"heap_mib"`
	SysMiB      uint64 `json:"sys_mib"`
	Connections int64  `json:"conns"`
}

// reportURLOverridePath returns ~/.urnetwork/report_url, a file an operator
// can write at any time to set or change the hub target without restarting
// the provider. It takes precedence over URNETWORK_REPORT_URL, which is read
// once at process start and otherwise can't be changed without a restart.
func reportURLOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "report_url"), nil
}

// resolveReportURL re-reads the override file on every call so a change
// takes effect on the reporter's next tick. envFallback is the value
// captured from URNETWORK_REPORT_URL at startup, used when no override file
// exists or it's empty.
func resolveReportURL(envFallback string) string {
	path, err := reportURLOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
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
// resolveReportURL, so writing ~/.urnetwork/report_url (or emptying it) turns
// reporting on, off, or repoints it at a different hub without a restart;
// envReportURL is only the startup-time fallback used when that file doesn't
// exist. It is a best-effort telemetry loop: failures are logged but never
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
	defer ticker.Stop()

	activeInterval := interval
	activeReportURL := ""
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
		if reportURL != activeReportURL {
			if reportURL == "" {
				tlog("[report] hub reporting disabled (node=%s)\n", nodeID)
			} else if apiURL, err := url.JoinPath(reportURL, "/api/report"); err == nil {
				tlog("[report] posting bandwidth to %s every %s (node=%s)\n", apiURL, activeInterval, nodeID)
			}
			activeReportURL = reportURL
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
		resp, err := newClientForURL(reportURL).Do(req)
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

		proxies = append(proxies, proxyReport{
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
		})
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
		},
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

func newClientForURL(reportURL string) *http.Client {
	if !strings.HasPrefix(reportURL, "https://") {
		return &http.Client{Timeout: 10 * time.Second}
	}

	pins := loadPinnedFPs()

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(pins) > 0 {
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("no peer certificates")
			}
			fp := fmt.Sprintf("SHA256:%x", sha256.Sum256(cs.PeerCertificates[0].Raw))
			if pins[fp] {
				return nil
			}
			err := fmt.Errorf("certificate fingerprint %s is not pinned", fp)
			os.WriteFile(filepath.Join(os.TempDir(), "hub-tls-debug.txt"), []byte(fmt.Sprintf("computed: %s\npinned: %v\nerror: %v\n", fp, pins, err)), 0644)
			return err
		}
	} else {
		tlsCfg.InsecureSkipVerify = true
	}

	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}
}
