package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/metrics"
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
	ID      string `json:"id"`
	Address string `json:"addr"`
	Status  string `json:"status"`
	TotalRX uint64 `json:"rx"`
	TotalTX uint64 `json:"tx"`
	BillRX  uint64 `json:"bill_rx"`
	BillTX  uint64 `json:"bill_tx"`
	Clients int64  `json:"clients"`
	MaxAge  int64  `json:"max_age_s"`
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
// retried beyond the next tick. The cadence defaults to 60s and is
// overridable via URNETWORK_REPORT_INTERVAL (min 10s). The bandwidthReport /
// proxyReport JSON shape mirrors what the hub decodes, so keep the json tags
// here in sync with hub/main.go.
func runBandwidthReporter(ctx context.Context, nodeID, host, envReportURL string, startTime time.Time) {
	interval := 60 * time.Second
	if s := os.Getenv("URNETWORK_REPORT_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 10*time.Second {
			interval = d
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}

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

	activeReportURL := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		reportURL := resolveReportURL(envReportURL)
		if reportURL != activeReportURL {
			if reportURL == "" {
				fmt.Printf("[report] hub reporting disabled (node=%s)\n", nodeID)
			} else if apiURL, err := url.JoinPath(reportURL, "/api/report"); err == nil {
				fmt.Printf("[report] posting bandwidth to %s every %s (node=%s)\n", apiURL, interval, nodeID)
			}
			activeReportURL = reportURL
		}
		if reportURL == "" {
			continue
		}

		apiURL, err := url.JoinPath(reportURL, "/api/report")
		if err != nil {
			fmt.Printf("[report] invalid report URL %s: %v\n", reportURL, err)
			continue
		}

		report := buildReport(nodeID, host, startTime)
		if len(report.Proxies) == 0 {
			fmt.Printf("[report] skipping (0 proxies in bandwidth map)\n")
			continue
		}
		fmt.Printf("[report] sending %d proxies to %s\n", len(report.Proxies), apiURL)

		body, err := json.Marshal(report)
		if err != nil {
			fmt.Printf("[report] marshal error: %v\n", err)
			continue
		}

		resp, err := client.Post(apiURL, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Printf("[report] post failed: %v\n", err)
			continue
		}
		// surface a rejecting hub instead of silently treating any response as
		// success. without this a 401/404/5xx looks identical to a 200 and the
		// fleet dashboard goes stale with no signal on the provider side. the
		// 60s cadence already rate-limits this, so log every occurrence.
		if resp.StatusCode/100 != 2 {
			fmt.Printf("[report] hub rejected report: %s\n", resp.Status)
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

		proxies = append(proxies, proxyReport{
			ID:      key,
			Address: ip,
			Status:  status,
			TotalRX: bw.TotalRx.Load(),
			TotalTX: bw.TotalTx.Load(),
			BillRX:  bw.BillableRx.Load(),
			BillTX:  bw.BillableTx.Load(),
			Clients: bw.Clients.Load(),
			MaxAge:  int64(bw.MaxAge().Seconds()),
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
