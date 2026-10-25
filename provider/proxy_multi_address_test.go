package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// Tests for the multi-address probe paths: a host the box resolves to more than
// one address is ONE sample, and every address is tried before the proxy or the
// route is convicted. Both properties were absent while the loops read
// resolveProbeTarget (ips[0]) only.

// tlsProbeEndpoint stands up a TLS listener with a throwaway certificate and
// returns its address, so a test can redirect the direct dialer to a handshake
// that either completes (skip-verify) or fails on the certificate.
func tlsProbeEndpoint(t *testing.T) string {
	t.Helper()
	cert, err := selfSignedForTest()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if tc, ok := c.(*tls.Conn); ok {
					if err := tc.Handshake(); err != nil {
						return
					}
				}
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// refusedProbeIP is the address the fake proxy answers REP 0x05 (connection
// refused) for, so a test can make the FIRST resolved address fail on purpose.
var refusedProbeIP = net.ParseIP("198.51.100.1")

// acceptedProbeIP is the address the fake proxy answers REP 0x00 for.
var acceptedProbeIP = net.ParseIP("203.0.113.7")

// listenSocks5RefusingFirst starts a fake SOCKS5 proxy that refuses (REP 0x05)
// CONNECTs to refusedProbeIP and accepts everything else. It returns the proxy
// address, a counter of refused CONNECTs and a counter of all CONNECTs.
func listenSocks5RefusingFirst(t *testing.T) (addr string, refused, dials *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	refusedCount, dialCount := new(int32), new(int32)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(c, greeting); err != nil {
					return
				}
				head := make([]byte, 4)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				var dst net.IP
				switch head[3] {
				case 0x01:
					buf := make([]byte, 6)
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					dst = net.IP(buf[:4])
				case 0x04:
					buf := make([]byte, 18)
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					dst = net.IP(buf[:16])
				default:
					return
				}
				atomic.AddInt32(dialCount, 1)
				rep := byte(0x00)
				if dst.Equal(refusedProbeIP) {
					atomic.AddInt32(refusedCount, 1)
					rep = 0x05
				}
				_, _ = c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()
	return ln.Addr().String(), refusedCount, dialCount
}

// TestProbeSocks5ConnectAnyTriesEveryAddress: the helper asks the proxy on each
// resolved address, stops at the first answer, and calls a host attempted when
// at least one dial left the box.
func TestProbeSocks5ConnectAnyTriesEveryAddress(t *testing.T) {
	addr, refused, dials := listenSocks5RefusingFirst(t)

	answered, attempted := probeSocks5ConnectAny(context.Background(), addr, "", "",
		[]net.IP{refusedProbeIP, acceptedProbeIP}, 443, time.Second)
	if !answered || !attempted {
		t.Fatalf("answered=%v attempted=%v, want both true when the second address answers", answered, attempted)
	}
	if got := atomic.LoadInt32(dials); got != 2 {
		t.Fatalf("dialed %d addresses, want 2 (the refused one, then the accepted one)", got)
	}
	if got := atomic.LoadInt32(refused); got != 1 {
		t.Fatalf("saw %d refusals, want 1", got)
	}

	// Every address refused: still attempted, and honestly not answered.
	answered, attempted = probeSocks5ConnectAny(context.Background(), addr, "", "",
		[]net.IP{refusedProbeIP}, 443, time.Second)
	if answered || !attempted {
		t.Fatalf("answered=%v attempted=%v, want answered=false attempted=true for a refused address", answered, attempted)
	}
}

// TestProbeTableThroughProxy_TriesEveryAddress: a name the box resolves to a
// refused address FIRST and a working one second must not be recorded as a
// failure. Before the fix the loop read ips[0] only, so every such name was
// convicted of a failure the proxy never committed.
func TestProbeTableThroughProxy_TriesEveryAddress(t *testing.T) {
	addr, _, dials := listenSocks5RefusingFirst(t)

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	names := namesInSample(addr, cfg)
	seedProbeDNS(t, names, []net.IP{refusedProbeIP, acceptedProbeIP})

	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	if len(names) == 0 {
		t.Skip("the sampled block contained no DNS names to seed")
	}
	failed := map[string]bool{}
	for _, f := range res.Failed {
		failed[f] = true
	}
	for _, h := range names {
		if failed[h] {
			t.Fatalf("host %q was recorded as a failure although its second address answers: failed=%v", h, res.Failed)
		}
	}
	if got := atomic.LoadInt32(dials); got < int32(len(names)) {
		t.Fatalf("only %d CONNECTs for %d multi-address hosts: the loop did not try a second address", got, len(names))
	}
}

// namesInSample returns the DNS names in the block the next table pass will
// sample for this proxy, mirroring the pass's own seed and counter.
func namesInSample(address string, cfg proxyTableProbeConfig) []string {
	var names []string
	for _, h := range sampleProbeHosts(tableProbeSeed(address, tableProbePassCounter.Load()), cfg.SampleWidth, cfg.UseSpreadOrder) {
		if net.ParseIP(h) == nil {
			names = append(names, h)
		}
	}
	return names
}

// seedProbeDNS points the given names at the given addresses and clears the
// entries on cleanup.
func seedProbeDNS(t *testing.T, names []string, ips []net.IP) {
	t.Helper()
	probeDNSCache.Lock()
	for _, h := range names {
		probeDNSCache.m[h] = probeDNSCachedIP{ips: ips, at: time.Now()}
		delete(probeDNSCache.fail, h)
	}
	probeDNSCache.Unlock()
	t.Cleanup(func() {
		probeDNSCache.Lock()
		defer probeDNSCache.Unlock()
		for _, h := range names {
			delete(probeDNSCache.m, h)
			delete(probeDNSCache.fail, h)
		}
	})
}

// TestProbeTableDirect_TriesEveryAddress: the direct pass must try the second
// address of a host whose first address does not dial. The dialer seam fails
// exactly one address, so the outcome isolates the loop.
func TestProbeTableDirect_TriesEveryAddress(t *testing.T) {
	withTempHome(t)
	cfg := directTestConfig(t)

	literals, names := directSampleHosts(cfg)
	if len(names) == 0 {
		t.Skip("the sampled block contained no DNS names to seed")
	}
	seedProbeDNS(t, names, []net.IP{refusedProbeIP, acceptedProbeIP})
	_ = literals

	// Fail the refused address and send everything else to the TLS endpoint the
	// config stands up. Only the multi-address loop can reach the second address.
	endpoint := tlsProbeEndpoint(t)
	saved := directDial
	directDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil && ip.Equal(refusedProbeIP) {
			return nil, fmt.Errorf("dial %s: connection refused", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", endpoint)
	}
	t.Cleanup(func() { directDial = saved })

	res := probeTableDirect(context.Background(), cfg)
	failed := map[string]bool{}
	for _, f := range res.Failed {
		failed[f] = true
	}
	for _, h := range names {
		if failed[h] {
			t.Fatalf("host %q failed although its second address is dialable: failed=%v", h, res.Failed)
		}
	}
	if res.OK < len(names) {
		t.Fatalf("ok=%d, want at least the %d seeded names reachable", res.OK, len(names))
	}
}

// TestProbeDirectTargetAnyKeepsAnInterception: an intercepted first address is
// not retracted by a second address that merely fails to dial.
func TestProbeDirectTargetAnyKeepsAnInterception(t *testing.T) {
	endpoint := tlsProbeEndpoint(t)

	savedDial := directDial
	savedTLS := directTLSConfig
	// Verification ON against a throwaway cert: the handshake fails with a
	// certificate error, which is the intercepted outcome.
	directTLSConfig = func(serverName string) *tls.Config {
		return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	dialed := new(int32)
	directDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.Equal(acceptedProbeIP) {
			return nil, fmt.Errorf("dial %s: no route to host", addr)
		}
		atomic.AddInt32(dialed, 1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", endpoint)
	}
	t.Cleanup(func() {
		directDial = savedDial
		directTLSConfig = savedTLS
	})

	outcome, attempted := probeDirectTargetAny(context.Background(), []net.IP{acceptedProbeIP, refusedProbeIP}, "example.com", 443, time.Second)
	if !attempted {
		t.Fatal("attempted=false, want true: the second address was dialed")
	}
	if outcome != directIntercepted {
		t.Fatalf("outcome = %v, want directIntercepted kept from the first address", outcome)
	}
}

// TestProbeSocks5ConnectAnyPartialSetCarriesNoEvidence: when the dial limiter
// denies a later address, the host was never fully asked, so a refusal on the
// first address is not a verdict about the proxy. Recording it as a failure was
// the defect code review found: the address that was never asked is the one that
// might have answered.
func TestProbeSocks5ConnectAnyPartialSetCarriesNoEvidence(t *testing.T) {
	addr, refused, _ := listenSocks5RefusingFirst(t)

	orig := globalProbeDialLimiter
	// Burst 1, rate 0: the first address takes the only token, the second waits
	// out the per-target timeout and is denied.
	globalProbeDialLimiter = rate.NewLimiter(rate.Limit(0), 1)
	t.Cleanup(func() { globalProbeDialLimiter = orig })

	answered, attempted := probeSocks5ConnectAny(context.Background(), addr, "", "",
		[]net.IP{refusedProbeIP, acceptedProbeIP}, 443, 50*time.Millisecond)
	if answered || attempted {
		t.Fatalf("answered=%v attempted=%v, want false/false: a partial set carries no evidence", answered, attempted)
	}
	if got := atomic.LoadInt32(refused); got != 1 {
		t.Fatalf("saw %d refusals, want 1 (only the first address should leave the box)", got)
	}
}

// TestProbeDirectTargetAnyPartialSetCarriesNoVerdict: the direct pass has its
// own bucket. A first address that fails while the limiter denies the second
// must not convict the route, for the same reason.
func TestProbeDirectTargetAnyPartialSetCarriesNoVerdict(t *testing.T) {
	orig := directProbeDialLimiter
	directProbeDialLimiter = rate.NewLimiter(rate.Limit(0), 1)
	t.Cleanup(func() { directProbeDialLimiter = orig })

	// Both are closed loopback ports: the first dial leaves the box and fails,
	// the second never gets a token.
	outcome, attempted := probeDirectTargetAny(context.Background(),
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2")}, "localhost", 1, 50*time.Millisecond)
	if attempted {
		t.Fatalf("attempted=true (outcome=%v) for a set the limiter cut short; a partial set must carry no verdict", outcome)
	}
	if outcome != directUnattempted {
		t.Fatalf("outcome=%v, want directUnattempted", outcome)
	}
}
