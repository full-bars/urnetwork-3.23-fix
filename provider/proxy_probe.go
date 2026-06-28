package main

import (
	"context"
	"encoding/binary"
	mathrand "math/rand"
	"net"
	"sync"
	"time"
)

// Probe result classification for the unified dual-stage probe.
type probeResult int

const (
	probeDead         probeResult = iota // TCP unreachable or not SOCKS5
	probeSocks5Only                      // speaks SOCKS5 but CONNECT to api failed
	probeAPIReachable                    // both SOCKS5 and API CONNECT succeeded
)

const (
	// proxyProbeTimeout bounds the SOCKS5 greeting phase (TCP + 2-byte exchange).
	proxyProbeTimeout = 3 * time.Second

	// proxyAPIAccessTimeout bounds the SOCKS5 CONNECT phase (through proxy to api).
	proxyAPIAccessTimeout = 5 * time.Second

	// proxyProbeConcurrency caps how many reachability probes run at once.
	proxyProbeConcurrency = 50

	// proxyProbeStagger is the max random jitter before each probe dial,
	// spreading the initial burst from a batch across a ~100ms window.
	proxyProbeStagger = 100 * time.Millisecond

	// proxyAPIMaxFails is the number of consecutive API probe failures before
	// an address is moved to the persistent Blacklist.
	proxyAPIMaxFails = 3

	// proxyReaperInterval is how often the background reaper scans the cache
	// for unproven or stale entries.
	proxyReaperInterval = 5 * time.Minute

	// proxyBlacklistCooldown is how long an address stays on the Blacklist
	// before the pruner removes it, giving it a chance to re-enter via a
	// fresh fetch cycle.
	proxyBlacklistCooldown = 24 * time.Hour

	// proxyBlacklistPruneInterval is how often the blacklist pruner runs.
	proxyBlacklistPruneInterval = 30 * time.Minute
)

// socks5Greeting is the client's opening message in the SOCKS5 handshake
// (RFC 1928 §3): version 5, offering exactly one auth method, "no
// authentication required" (0x00).
var socks5Greeting = []byte{0x05, 0x01, 0x00}

// socks5ConnectV4 builds a SOCKS5 CONNECT frame for an IPv4 destination.
// Format: VER(1) CMD(1) RSV(1) ATYP(1) DST.ADDR(4) DST.PORT(2)
func socks5ConnectV4(ip net.IP, port uint16) []byte {
	frame := make([]byte, 10)
	frame[0] = 0x05          // VER
	frame[1] = 0x01          // CMD = CONNECT
	frame[2] = 0x00          // RSV
	frame[3] = 0x01          // ATYP = IPv4
	copy(frame[4:8], ip.To4())
	binary.BigEndian.PutUint16(frame[8:10], port)
	return frame
}

// apiProbeAddr caches the resolved IP for api.bringyour.com so each probe
// doesn't trigger a fresh DNS lookup through every proxy.
var apiProbeAddr struct {
	mu    sync.Mutex
	ip    net.IP
	port  uint16
	host  string
}

func resolveAPIProbeAddr(host string, port uint16) (net.IP, uint16) {
	apiProbeAddr.mu.Lock()
	defer apiProbeAddr.mu.Unlock()
	if apiProbeAddr.ip != nil && apiProbeAddr.host == host && apiProbeAddr.port == port {
		return apiProbeAddr.ip, apiProbeAddr.port
	}
	ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, 0
	}
	apiProbeAddr.ip = ips[0].AsSlice()
	apiProbeAddr.port = port
	apiProbeAddr.host = host
	return apiProbeAddr.ip, apiProbeAddr.port
}

// probeProxy performs a two-stage check on a single proxy address:
//  1. SOCKS5 greeting (is this actually a SOCKS5 proxy?)
//  2. SOCKS5 CONNECT to api.bringyour.com:443 (can the proxy reach the API?)
//
// Both stages reuse one TCP connection. A random stagger up to
// proxyProbeStagger is applied before dialing to smooth batch bursts.
// The API destination IP is resolved once and cached across probes.
func probeProxy(ctx context.Context, address string, apiHost string, apiPort uint16) probeResult {
	stagger := time.Duration(mathrand.Intn(int(proxyProbeStagger)))
	timer := time.NewTimer(stagger)
	select {
	case <-ctx.Done():
		timer.Stop()
		return probeDead
	case <-timer.C:
	}

	// Stage 1: TCP connect + SOCKS5 greeting
	dialCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return probeDead
	}
	defer conn.Close()

	if deadline, ok := dialCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			tlog("[proxy][probe] warn: could not set stage-1 deadline: %v\n", err)
		}
	}

	if _, err := conn.Write(socks5Greeting); err != nil {
		return probeDead
	}

	greetingResp := make([]byte, 2)
	if _, err := conn.Read(greetingResp); err != nil {
		return probeDead
	}
	if greetingResp[0] != 0x05 {
		return probeDead
	}

	// Stage 2: SOCKS5 CONNECT to api.bringyour.com:443 (or custom apiHost)
	apiIP, apiPort := resolveAPIProbeAddr(apiHost, apiPort)
	if apiIP == nil {
		// DNS failed — can't probe, but the proxy is SOCKS5-reachable.
		// Return socks5-only so it isn't discarded; the reaper will retry.
		return probeSocks5Only
	}

	if err := conn.SetDeadline(time.Now().Add(proxyAPIAccessTimeout)); err != nil {
		tlog("[proxy][probe] warn: could not set stage-2 deadline: %v\n", err)
		return probeSocks5Only
	}
	connectFrame := socks5ConnectV4(apiIP, apiPort)
	if _, err := conn.Write(connectFrame); err != nil {
		return probeSocks5Only
	}

	connectResp := make([]byte, 10)
	if _, err := conn.Read(connectResp); err != nil {
		return probeSocks5Only
	}
	// Response: VER(1) REP(1) RSV(1) ATYP(1) BND.ADDR(4) BND.PORT(2)
	// REP = 0x00 means success
	if len(connectResp) >= 2 && connectResp[0] == 0x05 && connectResp[1] == 0x00 {
		return probeAPIReachable
	}
	return probeSocks5Only
}

// probeProxySocks5 is a light wrapper around probeProxy that returns true
// if the proxy completes a SOCKS5 greeting. It does not test API reachability —
// that is handled by the background reaper for cached entries and by the
// dual-stage probe during the URL fetch pipeline. Used at auth time as a
// cheap gate before spending an auth-rate-limiter slot.
func probeProxySocks5(ctx context.Context, address string, timeout time.Duration) bool {
	return probeProxy(ctx, address, "", 0) != probeDead
}

// probeAndFilterProxyURLLines parses each line, probes the address with the
// dual-stage check (SOCKS5 + API CONNECT), and returns only the lines whose
// probeResult is probeAPIReachable. Lines that fail to parse are dropped.
// Lines that reach SOCKS5 but fail API CONNECT are returned separately so
// the caller can cache them with ProbeOK=false for reaper retry.
func probeAndFilterProxyURLLines(ctx context.Context, lines []string, apiHost string, apiPort uint16) (apiOK, socks5Only []string) {
	type result struct {
		idx int
		r   probeResult
	}
	results := make([]result, len(lines))
	sem := make(chan struct{}, proxyProbeConcurrency)
	var wg sync.WaitGroup

	for i, line := range lines {
		address, _, _, ok := parseProxyURLLine(line)
		if !ok {
			results[i].r = probeDead
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, address string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = result{i, probeProxy(ctx, address, apiHost, apiPort)}
		}(i, address)
	}
	wg.Wait()

	for i, r := range results {
		switch r.r {
		case probeAPIReachable:
			apiOK = append(apiOK, lines[i])
		case probeSocks5Only:
			socks5Only = append(socks5Only, lines[i])
		}
	}
	return apiOK, socks5Only
}
