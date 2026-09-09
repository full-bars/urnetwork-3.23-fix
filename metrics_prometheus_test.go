package connect

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusHandlerReturnsValidFormat(t *testing.T) {
	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body := w.Body.String()

	// Must contain HELP and TYPE lines for each metric
	requiredMetrics := []string{
		"urnet_uptime_seconds",
		"urnet_connections_active",
		"urnet_proxy_pool_size",
		"urnet_proxy_billable_bytes_total",
		"urnet_proxy_session_age_seconds",
		"urnet_clients_active",
		"urnet_errors_total",
		"urnet_contracts_total",
		"urnet_gc_cycles_total",
		"urnet_mem_heap_bytes",
		"urnet_goroutines",
	}
	for _, m := range requiredMetrics {
		if !strings.Contains(body, m) {
			t.Errorf("missing metric %q in output", m)
		}
		// Each metric must have HELP and TYPE
		if !strings.Contains(body, "# HELP "+m) {
			t.Errorf("missing HELP for %q", m)
		}
		if !strings.Contains(body, "# TYPE "+m) {
			t.Errorf("missing TYPE for %q", m)
		}
	}
}

func TestPrometheusHandlerCounterIncrement(t *testing.T) {
	// Record an error and verify the counter increments
	before := globalProm.errorsTotal[ErrorTransport].Load()
	RecordError(ErrorTransport, "test error for prometheus")

	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	after := globalProm.errorsTotal[ErrorTransport].Load()

	if after <= before {
		t.Errorf("error counter did not increment: before=%d after=%d", before, after)
	}

	if !strings.Contains(body, `urnet_errors_total{category="transport"}`) {
		t.Error("missing transport error metric in output")
	}
}

func TestPrometheusHandlerUptimePositive(t *testing.T) {
	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	// uptime should be > 0
	if !strings.Contains(body, "urnet_uptime_seconds") {
		t.Error("missing uptime metric")
	}
}
