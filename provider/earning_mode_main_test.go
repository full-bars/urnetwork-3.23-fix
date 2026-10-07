package main

import "testing"

// TestEarningMode pins the three real transport shapes. All are reachable:
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
		{"neither yet", false, 0, "direct"},
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
