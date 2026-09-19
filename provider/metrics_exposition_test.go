//go:build linux

package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/promtext"
)

// TestProviderMetricsExpositionLints scrapes the handler exactly as a
// provider serves it (connect's families plus providerExtraMetrics) and runs
// the result through the text-format linter. An "info" type, a split family,
// a gauge named _total or a Go-only escape anywhere fails the whole scrape
// in Prometheus, so it fails here.
func TestProviderMetricsExpositionLints(t *testing.T) {
	withTempHome(t)
	connect.SetExtraMetricsProvider(providerExtraMetrics)
	t.Cleanup(func() { connect.SetExtraMetricsProvider(nil) })

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	connect.PrometheusHandler().ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "# TYPE urnet_info gauge") {
		t.Fatalf("provider families missing from the scrape")
	}
	for _, err := range promtext.Lint(body) {
		t.Error(err)
	}
}
