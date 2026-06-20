package main

import (
	"context"
	"net"
	"sync"
	"time"
)

// proxyProbeTimeout bounds how long a single reachability probe can take
// before the address is treated as unreachable.
const proxyProbeTimeout = 3 * time.Second

// proxyProbeConcurrency caps how many reachability probes run at once when
// probing a batch (e.g. a freshly fetched URL list), so checking a few
// thousand addresses can't itself become a bottleneck or look like a port
// scan to anything watching.
const proxyProbeConcurrency = 50

// probeProxyReachable does a bare TCP dial to address, returning true only
// if a connection is established within timeout. This only confirms the
// port accepts a connection — not that a SOCKS5 handshake or auth would
// succeed — but that's enough to filter out the bulk of dead entries
// cheaply, before they ever cost an auth attempt or a slot from the shared
// auth rate limiter.
func probeProxyReachable(ctx context.Context, address string, timeout time.Duration) bool {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// filterReachableProxyURLLines probes every line's address concurrently
// (bounded by proxyProbeConcurrency) and returns only the lines whose
// address is TCP-reachable. Lines that fail to parse are dropped too —
// mergeProxyURLEntries would skip them anyway, so there's no point spending
// a probe on something that was never going to be added.
func filterReachableProxyURLLines(ctx context.Context, lines []string) []string {
	reachable := make([]bool, len(lines))
	sem := make(chan struct{}, proxyProbeConcurrency)
	var wg sync.WaitGroup
	for i, line := range lines {
		address, _, _, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, address string) {
			defer wg.Done()
			defer func() { <-sem }()
			reachable[i] = probeProxyReachable(ctx, address, proxyProbeTimeout)
		}(i, address)
	}
	wg.Wait()

	result := make([]string, 0, len(lines))
	for i, ok := range reachable {
		if ok {
			result = append(result, lines[i])
		}
	}
	return result
}
