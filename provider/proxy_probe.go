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

// socks5Greeting is the client's opening message in the SOCKS5 handshake
// (RFC 1928 §3): version 5, offering exactly one auth method, "no
// authentication required" (0x00). We don't care whether the proxy actually
// requires credentials here — only whether something that speaks SOCKS5 is
// listening at all — so this is the cheapest greeting that gets a
// version-tagged response out of any real SOCKS5 server.
var socks5Greeting = []byte{0x05, 0x01, 0x00}

// probeProxySocks5 dials address and performs just the opening leg of the
// SOCKS5 handshake, returning true only if the response actually looks like
// a SOCKS5 server (version byte 0x05). A bare TCP probe alone can't tell a
// live SOCKS5 proxy apart from any other service that happens to accept
// connections on that port (an HTTP server, a dead stub that accepts but
// never replies, a captive portal) — those pass a TCP-only check but still
// waste a real auth attempt and a slot from the shared rate limiter once
// provideAuth actually tries to use them. This catches that class before it
// costs anything.
func probeProxySocks5(ctx context.Context, address string, timeout time.Duration) bool {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return false
	}
	defer conn.Close()

	if deadline, ok := dialCtx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if _, err := conn.Write(socks5Greeting); err != nil {
		return false
	}

	resp := make([]byte, 2)
	if _, err := conn.Read(resp); err != nil {
		return false
	}
	return resp[0] == 0x05
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
			reachable[i] = probeProxySocks5(ctx, address, proxyProbeTimeout)
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
