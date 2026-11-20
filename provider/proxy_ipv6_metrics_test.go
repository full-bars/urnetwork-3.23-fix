package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProxyIPv6Coverage(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"v6-yes-udp-yes": {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::1", At: now, Interval: time.Hour, UDP: "yes"}},
		"v6-yes-udp-no":  {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::2", At: now, Interval: time.Hour, UDP: "no"}},
		"v6-no":          {ipv6: proxyIPv6Result{Status: proxyIPv6No, At: now, Interval: time.Hour}},
		"v6-unknown":     {ipv6: proxyIPv6Result{Status: proxyIPv6Unknown, At: now, Interval: time.Hour}},
		"v6-expired":     {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::3", At: now.Add(-3 * time.Hour), Interval: time.Hour}},
		"running":        {done: make(chan struct{})},
		"offline":        {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, At: now, Interval: time.Hour}},
	}
	store.ipv6ProbesRun.Store(42)

	got := store.ipv6Coverage([]string{"v6-yes-udp-yes", "v6-yes-udp-no", "v6-no", "v6-unknown", "v6-expired", "running", "missing"}, now)
	want := &SnapshotProxyIPv6{
		Yes:        2,
		No:         1,
		Unknown:    1,
		NotProbed:  3,
		UDPYes:     1,
		UDPNo:      1,
		UDPUnknown: 0,
		Probes:     42,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestWriteIPv6ProxyMetrics(t *testing.T) {
	var b strings.Builder
	writeIPv6ProxyMetrics(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("expected nil cov to emit nothing, got: %s", b.String())
	}

	cov := &SnapshotProxyIPv6{
		Yes:        12,
		No:         988,
		Unknown:    4,
		NotProbed:  10,
		UDPYes:     10,
		UDPNo:      2,
		UDPUnknown: 0,
		Probes:     1004,
	}
	writeIPv6ProxyMetrics(&b, cov)
	out := b.String()

	expectedLines := []string{
		`# HELP urnet_proxy_ipv6_proxies Online proxies by cached IPv6 capability result.`,
		`# TYPE urnet_proxy_ipv6_proxies gauge`,
		`urnet_proxy_ipv6_proxies{state="yes"} 12`,
		`urnet_proxy_ipv6_proxies{state="no"} 988`,
		`urnet_proxy_ipv6_proxies{state="unknown"} 4`,
		`urnet_proxy_ipv6_proxies{state="not_probed_yet"} 10`,
		`# HELP urnet_proxy_ipv6_udp_proxies Online IPv6 proxies by cached UDP relay result.`,
		`# TYPE urnet_proxy_ipv6_udp_proxies gauge`,
		`urnet_proxy_ipv6_udp_proxies{state="yes"} 10`,
		`urnet_proxy_ipv6_udp_proxies{state="no"} 2`,
		`urnet_proxy_ipv6_udp_proxies{state="unknown"} 0`,
		`# HELP urnet_proxy_ipv6_probes_total Number of proxy IPv6 capability probes run.`,
		`# TYPE urnet_proxy_ipv6_probes_total counter`,
		`urnet_proxy_ipv6_probes_total 1004`,
	}
	for _, exp := range expectedLines {
		if !strings.Contains(out, exp) {
			t.Fatalf("missing expected metric line: %q in:\n%s", exp, out)
		}
	}
}

func TestSnapshotProxyIPv6JSON(t *testing.T) {
	orig := SnapshotProxies{
		Up: 10,
		IPv6: &SnapshotProxyIPv6{
			Yes:        12,
			No:         988,
			Unknown:    4,
			NotProbed:  0,
			UDPYes:     10,
			UDPNo:      2,
			UDPUnknown: 0,
			Probes:     1004,
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var dec SnapshotProxies
	if err := json.Unmarshal(data, &dec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, dec) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", dec, orig)
	}

	// Verify omitempty
	var empty SnapshotProxies
	data, _ = json.Marshal(empty)
	if strings.Contains(string(data), "ipv6") {
		t.Fatalf("expected nil IPv6 to be omitted: %s", string(data))
	}
}
