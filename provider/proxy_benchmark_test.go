package main

import (
	"os"
	"testing"
)

// TestResolveBenchmarkEndpoint is the regression for the CodeRabbit finding:
// resolveBenchmarkEndpoint manually trimmed the "wss://"/"ws://" scheme
// prefix instead of parsing the URL, so a saved connect_url containing a
// path (validateConnectUrl only requires a ws/wss scheme and a host, per
// network.go) left that path in the "cleaned" string, breaking
// net.SplitHostPort / producing a garbage port.
func TestResolveBenchmarkEndpoint(t *testing.T) {
	os.Unsetenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT")

	cases := []struct {
		name       string
		connectUrl string
		want       string
	}{
		{"no port, no path", "wss://connect.example.com", "connect.example.com:443"},
		{"explicit port", "wss://connect.example.com:8443", "connect.example.com:8443"},
		{"path, no port -- the regression case", "wss://connect.example.com/connect", "connect.example.com:443"},
		{"path and port", "wss://connect.example.com:8443/connect", "connect.example.com:8443"},
		{"ws scheme", "ws://connect.example.com", "connect.example.com:443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withTempHome(t)
			if err := writeNetworkConfig("https://api.example.com", c.connectUrl); err != nil {
				t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
			}
			got := resolveBenchmarkEndpoint()
			if got != c.want {
				t.Fatalf("resolveBenchmarkEndpoint() with connect_url=%q = %q, want %q", c.connectUrl, got, c.want)
			}
		})
	}
}

// TestResolveBenchmarkEndpointEnvOverride verifies the env var still wins
// over any saved network config.
func TestResolveBenchmarkEndpointEnvOverride(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://api.example.com", "wss://connect.example.com/connect"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	os.Setenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT", "override.example.com:1234")
	defer os.Unsetenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT")

	got := resolveBenchmarkEndpoint()
	if got != "override.example.com:1234" {
		t.Fatalf("resolveBenchmarkEndpoint() = %q, want env override %q", got, "override.example.com:1234")
	}
}
