package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/promtext"
)

func TestDirectSlotProxyCellDetailReplyShape(t *testing.T) {
	connect.ResetDirectDualStackForTest()
	defer connect.ResetDirectDualStackForTest()

	stats := connect.DirectDualStackStats()
	now := time.Unix(1732000000, 0)
	stats.RecordConnect(4, true, now)
	stats.AddRx(4, 500)
	stats.AddTx(4, 250)
	stats.RecordConnect(6, false, now)
	stats.AddRx(6, 100)

	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()

	// Register proxy 0 (direct) and proxy 1
	connect.RegisterProxy(0, "direct", "direct")
	connect.RegisterProxy(1, "192.0.2.1:1080", "192.0.2.1:1080")

	store := &proxyCellStore{prev: map[int]uint64{}}
	details := store.Detail(now)

	var directDetail, proxyDetail *proxyCellDetail
	for i := range details {
		if details[i].Index == 0 {
			directDetail = &details[i]
		} else if details[i].Index == 1 {
			proxyDetail = &details[i]
		}
	}

	if directDetail == nil {
		t.Fatal("direct detail (index 0) missing")
	}
	if directDetail.IPv4 == nil || directDetail.IPv6 == nil {
		t.Fatalf("direct detail missing IPv4 or IPv6: %+v", directDetail)
	}
	if directDetail.IPv4.ConnectsOK != 1 || directDetail.IPv4.RxBytes != 500 || directDetail.IPv4.TxBytes != 250 {
		t.Fatalf("unexpected direct IPv4 values: %+v", directDetail.IPv4)
	}
	if directDetail.IPv6.ConnectsFailed != 1 || directDetail.IPv6.RxBytes != 100 {
		t.Fatalf("unexpected direct IPv6 values: %+v", directDetail.IPv6)
	}

	// Proxy 1 must not have direct dual stack stats
	if proxyDetail == nil {
		t.Fatal("proxy 1 detail missing")
	}
	if proxyDetail.IPv4 != nil || proxyDetail.IPv6 != nil {
		t.Fatalf("proxy 1 must omit IPv4 and IPv6: %+v", proxyDetail)
	}

	// Verify JSON serialization omitempty
	b, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"ipv4":`) || !strings.Contains(jsonStr, `"ipv6":`) {
		t.Fatalf("marshaled json missing ipv4/ipv6 for direct slot: %s", jsonStr)
	}
	// proxy 1 section should not have ipv4
	var unmarshaled []map[string]any
	if err := json.Unmarshal(b, &unmarshaled); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	for _, m := range unmarshaled {
		idx, _ := m["index"].(float64)
		if idx == 1 {
			if _, ok := m["ipv4"]; ok {
				t.Fatalf("proxy 1 JSON must omit ipv4: %v", m)
			}
			if _, ok := m["ipv6"]; ok {
				t.Fatalf("proxy 1 JSON must omit ipv6: %v", m)
			}
		}
	}
}

func TestDirectSlotProxyCellDetailOldReplyCompatibility(t *testing.T) {
	// Old provider JSON response without ipv4 or ipv6 fields
	oldJSON := `[
		{"index": 0, "addr": "direct (this host)", "state": 0, "clients": 1, "interval_bytes": 0, "age_seconds": 60, "udp": "direct"},
		{"index": 1, "addr": "192.0.2.1:1080", "state": 0, "clients": 0, "interval_bytes": 0, "age_seconds": 30, "udp": "ok"}
	]`

	var details []proxyCellDetail
	if err := json.Unmarshal([]byte(oldJSON), &details); err != nil {
		t.Fatalf("unmarshal old reply failed: %v", err)
	}

	if len(details) != 2 {
		t.Fatalf("expected 2 details, got %d", len(details))
	}
	if details[0].Index != 0 || details[0].IPv4 != nil || details[0].IPv6 != nil {
		t.Fatalf("old direct detail should have nil IPv4 and IPv6: %+v", details[0])
	}
}

func TestDirectDualStackMetricsExposition(t *testing.T) {
	withTempHome(t)
	connect.ResetDirectDualStackForTest()
	defer connect.ResetDirectDualStackForTest()

	stats := connect.DirectDualStackStats()
	now := time.Unix(1732000000, 0)
	stats.RecordConnect(4, true, now)
	stats.RecordConnect(4, false, now)
	stats.AddRx(4, 500)
	stats.AddTx(4, 250)

	stats.RecordConnect(6, true, now)
	stats.RecordConnect(6, false, now)
	stats.AddRx(6, 1200)
	stats.AddTx(6, 800)

	connect.SetExtraMetricsProvider(providerExtraMetrics)
	t.Cleanup(func() { connect.SetExtraMetricsProvider(nil) })

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	connect.PrometheusHandler().ServeHTTP(w, req)
	body := w.Body.String()

	// Verify all metric families are present
	for _, metric := range []string{
		"urnet_direct_connects_total",
		"urnet_direct_connect_failures_total",
		"urnet_direct_bytes_total",
		"urnet_direct_last_success_timestamp_seconds",
	} {
		if !strings.Contains(body, "# TYPE "+metric+" ") {
			t.Errorf("missing metric %s from scrape", metric)
		}
	}

	// Verify labels and values (counts per family only, no IP addresses in metric lines or labels)
	if !strings.Contains(body, `urnet_direct_connects_total{family="ipv4"} 1`) {
		t.Errorf("missing expected ipv4 connects: %s", body)
	}
	if !strings.Contains(body, `urnet_direct_connects_total{family="ipv6"} 1`) {
		t.Errorf("missing expected ipv6 connects: %s", body)
	}
	if !strings.Contains(body, `urnet_direct_bytes_total{dir="rx",family="ipv4"} 500`) {
		t.Errorf("missing expected ipv4 rx bytes: %s", body)
	}
	if !strings.Contains(body, `urnet_direct_bytes_total{dir="tx",family="ipv6"} 800`) {
		t.Errorf("missing expected ipv6 tx bytes: %s", body)
	}
	if !strings.Contains(body, `urnet_direct_last_success_timestamp_seconds{family="ipv4"} 1732000000`) {
		t.Errorf("missing expected ipv4 timestamp: %s", body)
	}

	// Redaction: ensure no IP addresses or credentials appear anywhere in the direct metrics
	var directLines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "urnet_direct_") {
			directLines = append(directLines, line)
		}
	}
	directSection := strings.Join(directLines, "\n")
	for _, bad := range []string{"192.0.2.", "2001:db8:", "password", "token"} {
		if strings.Contains(directSection, bad) {
			t.Fatalf("direct metrics contain forbidden address or secret %q:\n%s", bad, directSection)
		}
	}

	// Run Prometheus linter
	for _, err := range promtext.Lint(body) {
		t.Error(err)
	}
}
