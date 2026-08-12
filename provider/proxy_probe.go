package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
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
	probeTLSFailed                       // CONNECT succeeded but TLS to api did not verify (MITM/intercepting)
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

// The stale re-probe threshold for ProbeOK=true entries (proxies that
// passed the initial dual-stage probe but may have died later — without a
// re-probe window they'd be invisible to the reaper until the slow give-up
// eviction pipeline caught them) now scales with pressure; see
// reaperStaleThreshold / reaperStaleCalm / reaperStaleHot in
// resource_pressure.go. reaperStaleCalm (3h) matches this comment's
// original fixed value, balancing catching dead proxies within the same day
// against not re-probing every entry on every 5-minute cycle.

// socks5Greeting is the client's opening message in the SOCKS5 handshake
// (RFC 1928 §3): version 5, offering exactly one auth method, "no
// authentication required" (0x00).
var socks5Greeting = []byte{0x05, 0x01, 0x00}

// socks5ConnectV4 builds a SOCKS5 CONNECT frame for an IPv4 destination.
// Format: VER(1) CMD(1) RSV(1) ATYP(1) DST.ADDR(4) DST.PORT(2)
func socks5ConnectV4(ip net.IP, port uint16) []byte {
	frame := make([]byte, 10)
	frame[0] = 0x05 // VER
	frame[1] = 0x01 // CMD = CONNECT
	frame[2] = 0x00 // RSV
	frame[3] = 0x01 // ATYP = IPv4
	copy(frame[4:8], ip.To4())
	binary.BigEndian.PutUint16(frame[8:10], port)
	return frame
}

// socks5Greet completes the SOCKS5 greeting phase on conn: it offers no-auth
// (0x00), plus username/password (0x02) when BOTH credentials are non-empty,
// then validates the server's method selection. Returns true only when the
// server selected a method the client actually offered and any RFC 1929
// sub-negotiation succeeded. A server that selects 0x02 without complete
// credentials being supplied, or any method we never offered (e.g. 0x01
// GSSAPI), is not a usable proxy — proceeding into CONNECT without finishing
// that method's negotiation would be a blind guess (review #3). RFC 1929
// bounds each credential at 255 bytes; longer credentials would truncate
// silently on the wire and make a working proxy look dead, so reject them
// outright.
func socks5Greet(conn net.Conn, user, password string) bool {
	if len(user) > 255 || len(password) > 255 {
		return false
	}
	greeting := socks5Greeting
	if user != "" && password != "" {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(greeting); err != nil {
		return false
	}
	greetingResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetingResp); err != nil {
		return false
	}
	if greetingResp[0] != 0x05 {
		return false
	}
	switch greetingResp[1] {
	case 0x00:
		// No auth selected — proceed.
		return true
	case 0x02:
		if user == "" || password == "" {
			// Server demands credentials the client did not fully supply.
			return false
		}
		// RFC 1929 sub-negotiation.
		auth := []byte{0x01, byte(len(user))}
		auth = append(auth, []byte(user)...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, []byte(password)...)
		if _, err := conn.Write(auth); err != nil {
			return false
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return false
		}
		return authResp[0] == 0x01 && authResp[1] == 0x00
	default:
		// A method we never offered, or 0xFF "no acceptable method".
		return false
	}
}

// readSocks5ConnectReply consumes and validates a SOCKS5 CONNECT reply:
// VER(1)=0x05, REP(1), RSV(1)=0x00, ATYP(1), then the BND.ADDR declared by
// ATYP (4 bytes IPv4, 1 length byte + name for domain, 16 bytes IPv6) and
// BND.PORT(2). Returns true only when the ENTIRE declared reply was read and
// REP == 0x00. A fixed-size read only completes an IPv4 reply: a short
// domain reply is shorter than 10 bytes (so io.ReadFull would block until
// the deadline) and an IPv6 reply is longer — either would misclassify a
// healthy proxy. Truncated, wrong-version, non-zero-RSV, or unsupported-ATYP
// replies are never an answer (review #9/10).
func readSocks5ConnectReply(conn net.Conn) bool {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return false
	}
	if header[0] != 0x05 || header[2] != 0x00 {
		return false
	}
	var addrLen int
	switch header[3] {
	case 0x01: // IPv4
		addrLen = 4
	case 0x03: // domain name
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			return false
		}
		addrLen = int(lenByte[0])
	case 0x04: // IPv6
		addrLen = 16
	default:
		return false
	}
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(conn, addr); err != nil {
		return false
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return false
	}
	return header[1] == 0x00
}

// apiProbeAddr caches the resolved IP for api.bringyour.com so each probe
// doesn't trigger a fresh DNS lookup through every proxy.
var apiProbeAddr struct {
	mu   sync.Mutex
	ip   net.IP
	port uint16
	host string
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

// proxyProbeTLSClientConfig builds the TLS client config used to verify the
// API connection through a candidate proxy. Production uses the system root
// pool with SNI pinned to the API host. Tests override it to inject a test
// CA (see proxy_probe_tls_test.go); the seam is restored per-test.
var proxyProbeTLSClientConfig = func(serverName string) *tls.Config {
	return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// probeProxy performs a two-stage check on a single proxy address:
//  1. SOCKS5 greeting (is this actually a SOCKS5 proxy?)
//  2. SOCKS5 CONNECT to api.bringyour.com:443 (can the proxy reach the API?)
//  3. TLS handshake through the proxy to the API host (does the proxy
//     relay TLS transparently, or does it terminate/intercept the
//     connection? A MITM proxy answers CONNECT with 0x00 like a real one
//     but presents its own certificate at the TLS layer — verification
//     fails and the proxy is classified probeTLSFailed, so an interceptor
//     is never admitted to the pool).
//
// Both stage 1 and stage 2 reuse one TCP connection; stage 3 (TLS) reuses
// the same tunnel. A random stagger up to proxyProbeStagger is applied
// before dialing to smooth batch bursts. The API destination IP is
// resolved once and cached across probes. Credentials (user/password) are
// honoured via RFC 1929 so credentialed entries are probed on the same
// terms the real auth path will use. Reads use io.ReadFull and the
// greeting method byte is inspected, so a partial reply or "no acceptable
// method" (0xFF) is not mistaken for a live proxy (finding H1).
func probeProxy(ctx context.Context, address, user, password string, apiHost string, apiPort uint16) probeResult {
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

	// Offer no-auth, plus username/password when we have BOTH creds.
	// socks5Greet validates the server's method selection (0x00, or 0x02 with
	// complete credentials) and runs the RFC 1929 sub-negotiation; a server
	// that picks a method we never offered is not a usable proxy (review #3).
	if !socks5Greet(conn, user, password) {
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

	// REP = 0x00 with a fully-consumed, well-formed reply means success. The
	// reply is parsed by ATYP (IPv4/domain/IPv6), not by a fixed length, so
	// a short domain reply or an IPv6 BND.ADDR is handled correctly
	// (review #9/10). Anything else — truncated, wrong version, REP != 0x00 —
	// means the API CONNECT failed: the proxy speaks SOCKS5 but cannot reach
	// the API, which is socks5-only.
	if !readSocks5ConnectReply(conn) {
		return probeSocks5Only
	}

	// Stage 3: TLS handshake through the proxy to the API host. A real
	// SOCKS5 proxy is transparent to TLS — the handshake bytes flow between
	// us and the API server untouched, so verification succeeds against the
	// server's real certificate. A MITM/intercepting proxy answers CONNECT
	// with 0x00 just like a real one (it passed stage 2) but terminates TLS
	// itself and presents its own certificate; verification fails and the
	// proxy is classified probeTLSFailed so it is never admitted to the
	// pool. The same tunnel is reused — no new TCP connection.
	tlsConn := tls.Client(conn, proxyProbeTLSClientConfig(apiHost))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return probeTLSFailed
	}
	return probeAPIReachable
}

// probeProxySocks5 is a light wrapper around probeProxy that returns true
// if the proxy completes a SOCKS5 greeting. It does not test API reachability —
// that is handled by the background reaper for cached entries and by the
// dual-stage probe during the URL fetch pipeline. Used at auth time as a
// cheap gate before spending an auth-rate-limiter slot.
func probeProxySocks5(ctx context.Context, address string, timeout time.Duration) bool {
	return probeProxy(ctx, address, "", "", "", 0) != probeDead
}

// probeAndFilterProxyURLLines parses each line, probes the address with the
// dual-stage check (SOCKS5 + API CONNECT), and returns only the lines whose
// probeResult is probeAPIReachable. Lines that fail to parse are dropped.
// Lines that reach SOCKS5 but fail API CONNECT are returned separately so
// the caller can cache them with ProbeOK=false for reaper retry. Credentials
// from the line (host:port:user:pass or socks5://user:pass@host:port) are
// carried into the probe so credentialed entries are evaluated on the same
// terms the real auth path uses (finding H3).
func probeAndFilterProxyURLLines(ctx context.Context, lines []string, apiHost string, apiPort uint16) (apiOK, socks5Only []string) {
	type result struct {
		idx int
		r   probeResult
	}
	results := make([]result, len(lines))
	// Pool is sized once per batch at batch start; a batch in flight keeps
	// its size even if pressure changes mid-batch.
	sem := make(chan struct{}, scaledProbeConcurrency(currentPressure()))
	var wg sync.WaitGroup

	for i, line := range lines {
		address, user, password, ok := parseProxyURLLine(line)
		if !ok {
			results[i].r = probeDead
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, address, user, password string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = result{i, probeProxy(ctx, address, user, password, apiHost, apiPort)}
		}(i, address, user, password)
	}
	wg.Wait()

	for i, r := range results {
		switch r.r {
		case probeAPIReachable:
			apiOK = append(apiOK, lines[i])
		case probeSocks5Only, probeTLSFailed:
			// probeTLSFailed (CONNECT ok, TLS verify failed = MITM/interceptor)
			// is surfaced through the same retry bucket so the reaper's
			// failure-count → blacklist lifecycle retires it — it must never
			// be admitted to the pool.
			socks5Only = append(socks5Only, lines[i])
		}
	}
	return apiOK, socks5Only
}
