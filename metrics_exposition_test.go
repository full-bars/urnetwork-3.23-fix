package connect

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/promtext"
)

func scrape(t *testing.T, acceptEncoding string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Header().Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		body = string(raw)
	}
	return w, body
}

// registerTestProxies adds proxies whose addresses need escaping, and removes
// them when the test ends so other tests see the registry unchanged.
func registerTestProxies(t *testing.T) {
	t.Helper()
	addrs := map[int]string{
		900001: `10.0.0.1:1080`,
		900002: `quote"back\slash:1080`,
		900003: "tab\there:1080",
	}
	for idx, addr := range addrs {
		RegisterProxy(idx, addr, addr)
		bw := RegisterProxyBandwidth(idx)
		bw.TotalRx.Add(100)
		bw.BillableTx.Add(7)
	}
	t.Cleanup(func() {
		for idx := range addrs {
			UnregisterProxy(idx)
		}
	})
}

// TestPrometheusExpositionLints runs the full handler output through the
// text-format linter with per-proxy series present: interleaved families,
// Go-only escapes and misnamed types all fail here.
func TestPrometheusExpositionLints(t *testing.T) {
	registerTestProxies(t)
	_, body := scrape(t, "")
	if !strings.Contains(body, `urnet_proxy_bytes_total{proxy="proxy[900002] (quote\"back\\slash:1080)",direction="in"} 100`) {
		t.Fatalf("escaped per-proxy sample missing from output")
	}
	for _, err := range promtext.Lint(body) {
		t.Error(err)
	}
}

func TestPrometheusHandlerGzip(t *testing.T) {
	w, body := scrape(t, "gzip")
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary = %q, want Accept-Encoding", w.Header().Get("Vary"))
	}
	if !strings.Contains(body, "# TYPE urnet_uptime_seconds gauge") {
		t.Fatalf("decompressed body is missing urnet_uptime_seconds")
	}

	w, body = scrape(t, "")
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q without Accept-Encoding, want none", got)
	}
	if !strings.Contains(body, "# TYPE urnet_uptime_seconds gauge") {
		t.Fatalf("plain body is missing urnet_uptime_seconds")
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                            false,
		"gzip":                        true,
		"GZIP":                        true,
		"identity":                    false,
		"deflate, gzip;q=0.8":         true,
		"gzip;q=0":                    false,
		"gzip; q=0.0, identity":       false,
		"*":                           true,
		"*;q=0":                       false,
		"br, identity;q=1, *;q=0.5":   true,
		"x-gzip":                      false,
		"gzip;q=0.001":                true,
		"  gzip  ;  q=1  ,  identity": true,
	}
	for header, want := range cases {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestPrometheusLabelValue(t *testing.T) {
	cases := map[string]string{
		"plain":                `"plain"`,
		`a"b`:                  `"a\"b"`,
		`a\b`:                  `"a\\b"`,
		"line\nbreak":          `"line\nbreak"`,
		"tab\tstays":           "\"tab\tstays\"",
		"caf\u00e9":            "\"caf\u00e9\"",
		"bad\xffutf8":          "\"bad\uFFFDutf8\"",
		`proxy[3] (1.2.3.4:5)`: `"proxy[3] (1.2.3.4:5)"`,
	}
	for in, want := range cases {
		if got := PrometheusLabelValue(in); got != want {
			t.Errorf("PrometheusLabelValue(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPromtextLintCatchesKnownBreakage(t *testing.T) {
	cases := map[string]string{
		"info type":      "# TYPE x_info info\nx_info{version=\"1\"} 1\n",
		"split family":   "# TYPE a gauge\na 1\n# TYPE b gauge\nb 1\na{x=\"y\"} 2\n",
		"go escape":      "# TYPE a gauge\na{x=\"\\t\"} 1\n",
		"gauge _total":   "# TYPE a_total gauge\na_total 1\n",
		"counter suffix": "# TYPE a counter\na 1\n",
		"no type":        "a 1\n",
		"duplicate":      "# TYPE a gauge\na{x=\"1\"} 1\na{x=\"1\"} 2\n",
	}
	for name, body := range cases {
		if len(promtext.Lint(body)) == 0 {
			t.Errorf("%s: Lint found nothing in %q", name, body)
		}
	}
	good := "# HELP a_total ok\n# TYPE a_total counter\na_total{x=\"q\\\"\\\\\\n\"} 1\n# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1\nh_sum 1\nh_count 1\n"
	for _, err := range promtext.Lint(good) {
		t.Errorf("valid exposition flagged: %v", err)
	}
}
