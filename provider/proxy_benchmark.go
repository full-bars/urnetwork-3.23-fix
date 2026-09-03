package main

import (
	"context"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// resolveBenchmarkEndpoint returns the SOCKS5 egress-probe target. The
// URNETWORK_PROXY_BENCHMARK_ENDPOINT env var wins; otherwise it follows the
// operator's chosen network (M6) — the connect URL host:port from the saved
// network config, falling back to the main network's connect endpoint.
func resolveBenchmarkEndpoint() string {
	if v := os.Getenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT"); v != "" {
		return v
	}
	connectUrl := DefaultConnectUrl
	if cfg, ok, err := readNetworkConfig(); err == nil && ok && cfg.ConnectUrl != "" {
		connectUrl = cfg.ConnectUrl
	}
	cleaned := strings.TrimPrefix(strings.TrimPrefix(connectUrl, "wss://"), "ws://")
	if cleaned == "" {
		return "connect.bringyour.com:443"
	}
	if _, _, err := net.SplitHostPort(cleaned); err != nil {
		// No port in URL, just a hostname — assume wss default port.
		cleaned = cleaned + ":443"
	}
	return cleaned
}

// benchmarkEndpoint is resolved lazily (not at package init) so the chosen
// network config exists by the time a probe runs. The env-var override keeps
// existing behavior for tests and bespoke deployments.
var benchmarkEndpoint = func() string {
	return resolveBenchmarkEndpoint()
}

// startProxyBenchmarks launches optional latency probes for one proxy, enabled
// only when URNETWORK_PROXY_BENCHMARK=true. It starts two independent probes
// whose results are stored on the proxy's ProxyBandwidth for the hub dashboard:
//   - runTCPLatencyProbe: a raw TCP dial to the proxy's own address (LatencyNs).
//   - runSocksLatencyProbe: a SOCKS5 CONNECT through the proxy to
//     URNETWORK_PROXY_BENCHMARK_ENDPOINT (SocksLatencyNs), i.e. end-to-end
//     egress latency via the proxy.
//
// Note this measures relayed-path latency and is distinct from the
// [net][s]select dur= field, which times the provider's own control-plane dials.
func startProxyBenchmarks(ctx context.Context, bw *connect.ProxyBandwidth, settings *connect.ProxySettings) {
	if os.Getenv("URNETWORK_PROXY_BENCHMARK") != "true" {
		return
	}
	tcpInterval := 5 * time.Minute
	socksInterval := 15 * time.Minute

	// random startup jitter so proxies don't thundering-herd the endpoint
	jitter := time.Duration(rand.Int63n(int64(tcpInterval)))

	go runTCPLatencyProbe(ctx, bw, settings.Address, tcpInterval, jitter)
	go runSocksLatencyProbe(ctx, bw, settings, benchmarkEndpoint(), socksInterval, jitter)
}

func runTCPLatencyProbe(ctx context.Context, bw *connect.ProxyBandwidth, addr string, interval time.Duration, jitter time.Duration) {
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			continue
		}
		conn.Close()
		bw.LatencyNs.Store(int64(time.Since(start)))
	}
}

func runSocksLatencyProbe(ctx context.Context, bw *connect.ProxyBandwidth, settings *connect.ProxySettings, endpoint string, interval time.Duration, jitter time.Duration) {
	dialer, err := proxy.SOCKS5(settings.Network, settings.Address, settings.Auth, nil)
	if err != nil {
		return
	}
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		start := time.Now()
		conn, err := dialer.Dial("tcp", endpoint)
		if err != nil {
			continue
		}
		conn.Close()
		bw.SocksLatencyNs.Store(int64(time.Since(start)))
	}
}
