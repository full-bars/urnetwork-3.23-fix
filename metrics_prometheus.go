package connect

import (
	"fmt"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// prometheusCounters holds cumulative counters that the Prometheus /metrics
// handler reads. The connect package increments them from hot paths; the
// provider package can also push via the exported functions.
type prometheusCounters struct {
	errorsTotal  map[ErrorCategory]*atomic.Int64
	contractsIn  atomic.Int64 // contracts acquired
	contractsOut atomic.Int64 // contracts denied
	bytesIn      atomic.Int64
	bytesOut     atomic.Int64
	startTime    time.Time

	mu   sync.Mutex
	once sync.Once
}

var globalProm = &prometheusCounters{
	errorsTotal: map[ErrorCategory]*atomic.Int64{
		ErrorTransport: {},
		ErrorIP:        {},
		ErrorProxy:     {},
		ErrorWebRTC:    {},
	},
	startTime: time.Now(),
}

func (pc *prometheusCounters) initErrors() {
	pc.once.Do(func() {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		for cat := range pc.errorsTotal {
			pc.errorsTotal[cat] = &atomic.Int64{}
		}
	})
}

// IncrError increments the cumulative error counter for a category.
func IncrError(cat ErrorCategory) {
	globalProm.initErrors()
	if c, ok := globalProm.errorsTotal[cat]; ok {
		c.Add(1)
	}
}

// IncrContractAcquired increments the contract-acquired counter.
func IncrContractAcquired() { globalProm.contractsIn.Add(1) }

// IncrContractDenied increments the contract-denied counter.
func IncrContractDenied() { globalProm.contractsOut.Add(1) }

// ExtraMetricsProvider is set by the provider package to inject metrics
// that only the provider has access to (PQE counts, grades, churn, etc.).
// Called on every /metrics scrape. Return nil to skip extra metrics.
type ExtraMetricsProvider func() string

var extraMetricsMu sync.RWMutex
var extraMetricsProvider ExtraMetricsProvider

// SetExtraMetricsProvider registers the provider-side callback.
func SetExtraMetricsProvider(fn ExtraMetricsProvider) {
	extraMetricsMu.Lock()
	defer extraMetricsMu.Unlock()
	extraMetricsProvider = fn
}

// PrometheusHandler returns an http.Handler that serves metrics in
// Prometheus text format (no library required).
func PrometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder
		uptime := time.Since(globalProm.startTime).Seconds()

		// --- Uptime ---
		fmt.Fprintf(&b, "# HELP urnet_uptime_seconds Provider uptime in seconds.\n")
		fmt.Fprintf(&b, "# TYPE urnet_uptime_seconds gauge\n")
		fmt.Fprintf(&b, "urnet_uptime_seconds %g\n", uptime)

		// --- Active connections ---
		fmt.Fprintf(&b, "# HELP urnet_connections_active Currently active connections.\n")
		fmt.Fprintf(&b, "# TYPE urnet_connections_active gauge\n")
		fmt.Fprintf(&b, "urnet_connections_active %d\n", ActiveConnectionCount())

		// --- Proxy pool (from ProxyHealthSnapshot) ---
		up, dead, degraded, bandwidth, connecting := ProxyHealthSnapshot()
		fmt.Fprintf(&b, "# HELP urnet_proxy_pool_size Proxy pool composition.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_pool_size gauge\n")
		fmt.Fprintf(&b, "urnet_proxy_pool_size{status=\"up\"} %d\n", up)
		fmt.Fprintf(&b, "urnet_proxy_pool_size{status=\"dead\"} %d\n", len(dead))
		fmt.Fprintf(&b, "urnet_proxy_pool_size{status=\"degraded\"} %d\n", len(degraded))
		fmt.Fprintf(&b, "urnet_proxy_pool_size{status=\"connecting\"} %d\n", len(connecting))

		// --- Per-proxy bandwidth + provider aggregates ---
		fmt.Fprintf(&b, "# HELP urnet_proxy_bytes_total Cumulative bytes transferred per proxy.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_bytes_total counter\n")
		fmt.Fprintf(&b, "# HELP urnet_proxy_billable_bytes_total Cumulative billable bytes per proxy.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_billable_bytes_total counter\n")
		fmt.Fprintf(&b, "# HELP urnet_proxy_clients Active clients per proxy.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_clients gauge\n")
		fmt.Fprintf(&b, "# HELP urnet_proxy_session_age_seconds How long the current client presence window has been active per proxy.\n")
		fmt.Fprintf(&b, "# TYPE urnet_proxy_session_age_seconds gauge\n")
		fmt.Fprintf(&b, "# HELP urnet_billable_bytes_total Aggregate billable bytes for this provider.\n")
		fmt.Fprintf(&b, "# TYPE urnet_billable_bytes_total counter\n")
		var totalBillRx, totalBillTx uint64
		var totalRx, totalTx uint64
		var totalClients int64
		for key, bw := range bandwidth {
			rx := bw.TotalRx.Load()
			tx := bw.TotalTx.Load()
			billRx := bw.BillableRx.Load()
			billTx := bw.BillableTx.Load()
			clients := bw.Clients.Load()
			proxyID := escapeLabelValue(key)
			fmt.Fprintf(&b, "urnet_proxy_bytes_total{proxy=%q,direction=\"in\"} %d\n", proxyID, rx)
			fmt.Fprintf(&b, "urnet_proxy_bytes_total{proxy=%q,direction=\"out\"} %d\n", proxyID, tx)
			fmt.Fprintf(&b, "urnet_proxy_billable_bytes_total{proxy=%q,direction=\"in\"} %d\n", proxyID, billRx)
			fmt.Fprintf(&b, "urnet_proxy_billable_bytes_total{proxy=%q,direction=\"out\"} %d\n", proxyID, billTx)
			fmt.Fprintf(&b, "urnet_proxy_clients{proxy=%q} %d\n", proxyID, clients)
			age := bw.MaxAge()
			fmt.Fprintf(&b, "urnet_proxy_session_age_seconds{proxy=%q} %g\n", proxyID, age.Seconds())
			totalBillRx += billRx
			totalBillTx += billTx
			totalRx += rx
			totalTx += tx
			totalClients += clients
		}
		fmt.Fprintf(&b, "urnet_billable_bytes_total{direction=\"in\"} %d\n", totalBillRx)
		fmt.Fprintf(&b, "urnet_billable_bytes_total{direction=\"out\"} %d\n", totalBillTx)
		fmt.Fprintf(&b, "# HELP urnet_bytes_total Aggregate total bytes for this provider.\n")
		fmt.Fprintf(&b, "# TYPE urnet_bytes_total counter\n")
		fmt.Fprintf(&b, "urnet_bytes_total{direction=\"in\"} %d\n", totalRx)
		fmt.Fprintf(&b, "urnet_bytes_total{direction=\"out\"} %d\n", totalTx)
		fmt.Fprintf(&b, "# HELP urnet_clients_active Aggregate active clients across all proxies.\n")
		fmt.Fprintf(&b, "# TYPE urnet_clients_active gauge\n")
		fmt.Fprintf(&b, "urnet_clients_active %d\n", totalClients)

		// --- Error counters ---
		globalProm.initErrors()
		fmt.Fprintf(&b, "# HELP urnet_errors_total Cumulative transport errors by category.\n")
		fmt.Fprintf(&b, "# TYPE urnet_errors_total counter\n")
		for cat, c := range globalProm.errorsTotal {
			fmt.Fprintf(&b, "urnet_errors_total{category=%q} %d\n", string(cat), c.Load())
		}

		// --- Contract counters ---
		fmt.Fprintf(&b, "# HELP urnet_contracts_total Cumulative contract outcomes.\n")
		fmt.Fprintf(&b, "# TYPE urnet_contracts_total counter\n")
		fmt.Fprintf(&b, "urnet_contracts_total{result=\"acquired\"} %d\n", globalProm.contractsIn.Load())
		fmt.Fprintf(&b, "urnet_contracts_total{result=\"denied\"} %d\n", globalProm.contractsOut.Load())

		// --- Runtime metrics (GC, heap) ---
		gcSamples := []metrics.Sample{
			{Name: "/gc/cycles/total:gc-cycles"},
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/total:bytes"},
		}
		metrics.Read(gcSamples)

		fmt.Fprintf(&b, "# HELP urnet_gc_cycles_total Cumulative GC cycles.\n")
		fmt.Fprintf(&b, "# TYPE urnet_gc_cycles_total counter\n")
		fmt.Fprintf(&b, "urnet_gc_cycles_total %d\n", readUint64Metric(gcSamples[0].Value))

		fmt.Fprintf(&b, "# HELP urnet_mem_heap_bytes Go heap allocation in bytes.\n")
		fmt.Fprintf(&b, "# TYPE urnet_mem_heap_bytes gauge\n")
		fmt.Fprintf(&b, "urnet_mem_heap_bytes %d\n", readUint64Metric(gcSamples[1].Value))

		fmt.Fprintf(&b, "# HELP urnet_mem_sys_bytes Go runtime total memory obtained from OS.\n")
		fmt.Fprintf(&b, "# TYPE urnet_mem_sys_bytes gauge\n")
		fmt.Fprintf(&b, "urnet_mem_sys_bytes %d\n", readUint64Metric(gcSamples[2].Value))

		// --- Goroutines ---
		fmt.Fprintf(&b, "# HELP urnet_goroutines Number of goroutines.\n")
		fmt.Fprintf(&b, "# TYPE urnet_goroutines gauge\n")
		fmt.Fprintf(&b, "urnet_goroutines %d\n", runtime.NumGoroutine())

		// --- Enhanced pool metrics ---
		pool := EnhancedMetrics()
		if poolLatency, ok := pool["pool_latency_ms"].(float64); ok {
			fmt.Fprintf(&b, "# HELP urnet_pool_latency_ms Message pool average latency.\n")
			fmt.Fprintf(&b, "# TYPE urnet_pool_latency_ms gauge\n")
			fmt.Fprintf(&b, "urnet_pool_latency_ms %g\n", poolLatency)
		}

		// --- Provider-injected extra metrics (PQE, grades, churn) ---
		extraMetricsMu.RLock()
		provider := extraMetricsProvider
		extraMetricsMu.RUnlock()
		if provider != nil {
			if extra := provider(); extra != "" {
				b.WriteString(extra)
			}
		}

		_, _ = w.Write([]byte(b.String()))
	})
}

// readUint64Metric extracts a uint64 from a runtime/metrics Value.
func readUint64Metric(v metrics.Value) uint64 {
	switch v.Kind() {
	case metrics.KindUint64:
		return v.Uint64()
	case metrics.KindFloat64:
		return uint64(v.Float64())
	default:
		return 0
	}
}

// escapeLabelValue escapes backslash, double-quote, and newlines for
// Prometheus label values (per the text format spec).
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// FormatDuration formats seconds into human-readable units like "2d 5h 30m".
func FormatDuration(seconds float64) string {
	d := int(seconds) / 86400
	h := (int(seconds) % 86400) / 3600
	m := (int(seconds) % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", int(seconds))
	}
}
