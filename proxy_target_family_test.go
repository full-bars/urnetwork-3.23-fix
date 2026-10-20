package connect

// proxy_target_family_test.go — the per-proxy dual-stack workstream.
//
// The tests here dial through a loopback SOCKS5 stub, so the CONNECT frame is
// observable on the wire. That is the only place the difference between the two
// modes is visible: in v4 mode the stub is handed an IPv4 literal the provider
// resolved itself, and in proxy mode it is handed the destination NAME, which
// it resolves with an answer that has only an AAAA record.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestParseProxyTargetFamily(t *testing.T) {
	good := map[string]ProxyTargetFamilyMode{
		"":       ProxyTargetFamilyV4,
		"v4":     ProxyTargetFamilyV4,
		"V4":     ProxyTargetFamilyV4,
		" ipv4 ": ProxyTargetFamilyV4,
		"proxy":  ProxyTargetFamilyProxy,
		"PROXY":  ProxyTargetFamilyProxy,
	}
	for value, want := range good {
		got, err := ParseProxyTargetFamily(value)
		if err != nil {
			t.Fatalf("ParseProxyTargetFamily(%q) = %v, %v; want %v", value, got, err, want)
		}
		if got != want {
			t.Fatalf("ParseProxyTargetFamily(%q) = %v; want %v", value, got, want)
		}
		if got.String() != want.String() {
			t.Fatalf("ParseProxyTargetFamily(%q).String() = %q; want %q", value, got.String(), want.String())
		}
	}
	for _, bad := range []string{"v6", "ipv6", "on", "off", "1", "domain"} {
		if _, err := ParseProxyTargetFamily(bad); err == nil {
			t.Fatalf("ParseProxyTargetFamily(%q) must be rejected", bad)
		}
	}
}

// TestProxyTargetFamilyDefaultsToV4 is the rollout-safety assertion: the zero
// value of the process-global must be v4, so a build that never calls the
// setter behaves exactly as every release before the setting did.
func TestProxyTargetFamilyDefaultsToV4(t *testing.T) {
	previous := SetProxyTargetFamily(ProxyTargetFamilyV4)
	defer SetProxyTargetFamily(previous)

	if got := ProxyTargetFamily(); got != ProxyTargetFamilyV4 {
		t.Fatalf("default mode = %v; want v4", got)
	}
	if ProxyTargetFamilyCarriesName() {
		t.Fatal("v4 mode must resolve the target locally")
	}
	if previous := SetProxyTargetFamily(ProxyTargetFamilyProxy); previous != ProxyTargetFamilyV4 {
		t.Fatalf("Set returned previous %v; want v4", previous)
	}
	if !ProxyTargetFamilyCarriesName() {
		t.Fatal("proxy mode must carry the name")
	}
	// a value this build does not know reads as the default, not as an error
	if previous := SetProxyTargetFamily(ProxyTargetFamilyMode(42)); previous != ProxyTargetFamilyProxy {
		t.Fatalf("Set returned previous %v; want proxy", previous)
	}
	if got := ProxyTargetFamily(); got != ProxyTargetFamilyV4 {
		t.Fatalf("unknown mode landed on %v; want v4", got)
	}
}

// TestProxyModeSendsTheDestinationName is the workstream's headline test: the
// destination's resolution offers only an AAAA, the provider must not rewrite
// it, and the dial must succeed because the stub resolves the name to that
// AAAA.
func TestProxyModeSendsTheDestinationName(t *testing.T) {
	resetDnsCache()
	previous := SetProxyTargetFamily(ProxyTargetFamilyProxy)
	defer SetProxyTargetFamily(previous)

	dest, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %s", err)
	}
	defer dest.Close()
	_, destPort, err := net.SplitHostPort(dest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	const name = "aaaa-only.example"
	go serveOneReady(dest)

	proxyAddr, seen, stop := startSocks5Stub(t, func(req socks5Connect) (string, error) {
		if req.atyp != 0x03 {
			return "", fmt.Errorf("CONNECT carried address type 0x%02x, not a domain name", req.atyp)
		}
		if req.host != name {
			return "", fmt.Errorf("CONNECT carried %q; want %q", req.host, name)
		}
		// the stub's resolver has one record for this name and it is an AAAA
		return net.JoinHostPort("::1", req.port), nil
	})
	defer stop()

	conn, err := dialThroughProxy(proxyAddr, net.JoinHostPort(name, destPort))
	if err != nil {
		t.Fatalf("proxy mode dial of an AAAA-only name failed: %v", err)
	}
	defer conn.Close()

	if got := readAll(t, conn); got != "ready" {
		t.Fatalf("destination said %q; want %q", got, "ready")
	}

	req := awaitSocks5Request(t, seen)
	if req.atyp != 0x03 || req.host != name || req.port != destPort {
		t.Fatalf("CONNECT = atyp 0x%02x host %q port %q; want a domain %q port %q",
			req.atyp, req.host, req.port, name, destPort)
	}

	// the local resolution path, and the cache that backs it, must not run at
	// all in proxy mode: there is no address here to choose or to hold against a
	// target's health
	if _, ok := dnsCacheLookup(name); ok {
		t.Fatal("proxy mode populated the proxy-target DNS cache")
	}
}

// TestV4ModeSendsTheIpv4Literal pins today's behaviour: the default mode still
// resolves the target here with an A-only query, sends the proxy the IPv4
// literal, and fills the cache. If this test ever needs to change, the default
// changed and the rollout story is broken.
func TestV4ModeSendsTheIpv4Literal(t *testing.T) {
	resetDnsCache()
	previous := SetProxyTargetFamily(ProxyTargetFamilyV4)
	defer SetProxyTargetFamily(previous)

	dest, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	_, destPort, err := net.SplitHostPort(dest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	go serveOneReady(dest)

	proxyAddr, seen, stop := startSocks5Stub(t, func(req socks5Connect) (string, error) {
		if req.atyp != 0x01 {
			return "", fmt.Errorf("CONNECT carried address type 0x%02x, not an IPv4 literal", req.atyp)
		}
		return net.JoinHostPort(req.host, req.port), nil
	})
	defer stop()

	conn, err := dialThroughProxy(proxyAddr, net.JoinHostPort("localhost", destPort))
	if err != nil {
		t.Fatalf("v4 mode dial failed: %v", err)
	}
	defer conn.Close()

	if got := readAll(t, conn); got != "ready" {
		t.Fatalf("destination said %q; want %q", got, "ready")
	}

	req := awaitSocks5Request(t, seen)
	if req.atyp != 0x01 || req.host != "127.0.0.1" || req.port != destPort {
		t.Fatalf("CONNECT = atyp 0x%02x host %q port %q; want IPv4 127.0.0.1:%s",
			req.atyp, req.host, req.port, destPort)
	}
	if _, ok := dnsCacheLookup("localhost"); !ok {
		t.Fatal("v4 mode did not resolve and cache the target")
	}
}

// TestProxyModeLeavesIpLiteralTargetsAlone: a target that is already a literal
// has no name to carry, in either mode.
func TestProxyModeLeavesIpLiteralTargetsAlone(t *testing.T) {
	resetDnsCache()
	previous := SetProxyTargetFamily(ProxyTargetFamilyProxy)
	defer SetProxyTargetFamily(previous)

	dest, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	destHost, destPort, err := net.SplitHostPort(dest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	go serveOneReady(dest)

	proxyAddr, seen, stop := startSocks5Stub(t, func(req socks5Connect) (string, error) {
		if req.atyp != 0x01 {
			return "", fmt.Errorf("CONNECT carried address type 0x%02x", req.atyp)
		}
		return net.JoinHostPort(req.host, req.port), nil
	})
	defer stop()

	conn, err := dialThroughProxy(proxyAddr, net.JoinHostPort(destHost, destPort))
	if err != nil {
		t.Fatalf("literal target dial failed: %v", err)
	}
	defer conn.Close()
	if got := readAll(t, conn); got != "ready" {
		t.Fatalf("destination said %q; want %q", got, "ready")
	}
	req := awaitSocks5Request(t, seen)
	if req.atyp != 0x01 || req.host != destHost {
		t.Fatalf("CONNECT = atyp 0x%02x host %q; want IPv4 %q", req.atyp, req.host, destHost)
	}
	if _, ok := dnsCacheLookup(destHost); ok {
		t.Fatal("a literal target was put in the DNS cache")
	}
}

// dialThroughProxy is one proxied dial with the same bound a caller would give
// it.
func dialThroughProxy(proxyAddr string, target string) (net.Conn, error) {
	settings := &ProxySettings{Network: "tcp", Address: proxyAddr}
	dial := settings.NewDialContext(context.Background(), &net.Dialer{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return dial(ctx, "tcp", target)
}

// serveOneReady accepts one connection, writes a marker, and drains it. It
// stands in for a destination that is reachable and answers.
func serveOneReady(ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ready")); err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, conn)
}

func readAll(t *testing.T, conn net.Conn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("ready"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from the destination: %v", err)
	}
	return string(buf)
}

func dnsCacheLookup(host string) (dnsCacheEntry, bool) {
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	entry, ok := dnsCache.m[host]
	return entry, ok
}

// socks5Connect is one CONNECT frame as the stub read it.
type socks5Connect struct {
	atyp byte
	host string
	port string
}

// startSocks5Stub runs a loopback SOCKS5 server that answers the greeting with
// "no auth" and hands every CONNECT to dialTarget. A dialTarget error, or a
// target the stub cannot reach, is answered with REP 0x05 (connection refused),
// so a test that expected the name and got a literal fails the dial rather than
// hanging. A nil dialTarget holds the connection open with no reply, which is
// how a test makes the client's own deadline fire.
func startSocks5Stub(t *testing.T, dialTarget func(socks5Connect) (string, error)) (addr string, seen <-chan socks5Connect, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan socks5Connect, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSocks5(conn, dialTarget, requests)
		}
	}()
	return ln.Addr().String(), requests, func() { ln.Close() }
}

func serveSocks5(conn net.Conn, dialTarget func(socks5Connect) (string, error), requests chan<- socks5Connect) {
	defer conn.Close()

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	req := socks5Connect{atyp: head[3]}
	switch req.atyp {
	case 0x01:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		req.host = net.IP(raw).String()
	case 0x04:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		req.host = net.IP(raw).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		req.host = string(name)
	default:
		_, _ = conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	req.port = strconv.Itoa(int(portBytes[0])<<8 | int(portBytes[1]))

	select {
	case requests <- req:
	default:
	}

	if dialTarget == nil {
		// hold the connection open and say nothing: the client's own deadline is
		// the failure under test
		_, _ = io.Copy(io.Discard, conn)
		return
	}

	target, err := dialTarget(req)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

func awaitSocks5Request(t *testing.T, seen <-chan socks5Connect) socks5Connect {
	t.Helper()
	select {
	case req := <-seen:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("the stub saw no CONNECT")
		return socks5Connect{}
	}
}

// A proxied dial failure is wrapped so the family ledger can ignore it, and the
// wrapper must not change how the error classifies: code that asserts the
// concrete error to net.Error has to keep seeing timeouts as timeouts.
func TestProxyDialErrorPreservesNetErrorClassification(t *testing.T) {
	timedOut := &net.OpError{Op: "dial", Net: "tcp", Err: proxyDialTimeoutError{}}
	wrapped := &proxyDialError{index: 2, err: timedOut}
	if !wrapped.Timeout() {
		t.Fatal("a timed-out proxied dial must still read as a timeout")
	}
	var netErr net.Error
	if !errors.As(wrapped, &netErr) || !netErr.Timeout() {
		t.Fatal("errors.As must find net.Error through the proxy marker")
	}
	if wrapped.Error() != timedOut.Error() {
		t.Fatalf("message changed: %q != %q", wrapped.Error(), timedOut.Error())
	}

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if (&proxyDialError{err: refused}).Timeout() {
		t.Fatal("a refused proxied dial is not a timeout")
	}
}

// proxyDialTimeoutError is a net.Error that reports a timeout and nothing else.
type proxyDialTimeoutError struct{}

func (proxyDialTimeoutError) Error() string   { return "i/o timeout" }
func (proxyDialTimeoutError) Timeout() bool   { return true }
func (proxyDialTimeoutError) Temporary() bool { return true }
