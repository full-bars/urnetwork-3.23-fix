package main

import (
	connect "github.com/urnetwork/connect"
	"testing"
)

// TestEarningMode pins the four live transport states. All are reachable:
// direct can be turned off entirely (isDirectEnabled honours
// DISABLE_DIRECT_IP=1), so "proxies" is a genuine state and not a placeholder.
func TestEarningMode(t *testing.T) {
	cases := []struct {
		name      string
		directUp  bool
		proxiesUp int
		want      string
	}{
		{"direct only", true, 0, "direct"},
		{"pool only", false, 12, "proxies"},
		{"both", true, 12, "mixed"},
		{"neither up", false, 0, "none"},
	}
	for _, tc := range cases {
		if got := earningMode(tc.directUp, tc.proxiesUp); got != tc.want {
			t.Errorf("earningMode(direct=%v up=%d) = %q, want %q",
				tc.directUp, tc.proxiesUp, got, tc.want)
		}
	}
}

// TestIsDirectAddr covers the sentinel key and the port-suffixed form, plus a
// real proxy address that merely starts with the same letters.
func TestIsDirectAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"direct", true},
		{"direct:1080", true},
		// The shape ProxyHealthSnapshot actually returns (formatProxyEntry).
		// Missed on the first attempt, which left a direct-only box reporting
		// proxies_up=1 in live verification.
		{"proxy[0] (direct)", true},
		{"proxy[12] (1.2.3.4:1080)", false},
		{"1.2.3.4:1080", false},
		{"directproxy.example:1080", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isDirectAddr(tc.addr); got != tc.want {
			t.Errorf("isDirectAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestProfitModeNote pins the "say it only when it adds information" rule: a
// pool with proxies up gets NO tail, so the common healthy line stays clean.
func TestProfitModeNote(t *testing.T) {
	cases := []struct {
		mode      string
		proxiesUp int
		want      string
	}{
		{"direct", 0, " (direct mode, no proxies configured)"},
		{"mixed", 0, " (direct on, no proxies up yet)"},
		{"proxies", 0, " (no proxies up yet)"},
		{"proxies", 12, ""},
		{"mixed", 12, ""},
		{"", 0, ""},
	}
	for _, tc := range cases {
		if got := profitModeNote(tc.mode, tc.proxiesUp); got != tc.want {
			t.Errorf("profitModeNote(%q, %d) = %q, want %q",
				tc.mode, tc.proxiesUp, got, tc.want)
		}
	}
}

func TestProfitTransportCounts(t *testing.T) {
	direct := &connect.ProxyBandwidth{}
	direct.Clients.Store(2)
	direct.BillableRx.Store(42)
	proxy := &connect.ProxyBandwidth{}
	bw := map[string]*connect.ProxyBandwidth{"proxy[0] (direct)": direct, "proxy[1] (proxy)": proxy}
	for _, tc := range []struct {
		health   string
		up       int
		directUp bool
	}{
		{"up", 2, true}, {"dead", 1, false}, {"connecting", 1, false}, {"degraded", 1, false},
	} {
		t.Run(tc.health, func(t *testing.T) {
			health := map[string]connect.ProxyHealthStatus{directProxyKey: {Health: tc.health}}
			billable, clients, serving, proxies, directUp := profitTransportCounts(tc.up, bw, health)
			if billable != 42 || clients != 2 || serving != 0 || proxies != 1 || directUp != tc.directUp {
				t.Fatalf("got bytes=%d clients=%d serving=%d proxies=%d direct=%v", billable, clients, serving, proxies, directUp)
			}
		})
	}
}
