package main

import (
	"context"
	"math/rand"
	"net"
	"os"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

var benchmarkEndpoint = func() string {
	if v := os.Getenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT"); v != "" {
		return v
	}
	return "connect.bringyour.com:443"
}()

func startProxyBenchmarks(ctx context.Context, bw *connect.ProxyBandwidth, settings *connect.ProxySettings) {
	if os.Getenv("URNETWORK_PROXY_BENCHMARK") != "true" {
		return
	}
	tcpInterval := 5 * time.Minute
	socksInterval := 15 * time.Minute

	// random startup jitter so proxies don't thundering-herd the endpoint
	jitter := time.Duration(rand.Int63n(int64(tcpInterval)))

	go runTCPLatencyProbe(ctx, bw, settings.Address, tcpInterval, jitter)
	go runSocksLatencyProbe(ctx, bw, settings, benchmarkEndpoint, socksInterval, jitter)
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
