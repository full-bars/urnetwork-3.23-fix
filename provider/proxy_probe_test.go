package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// listenSocks5Once starts a TCP listener that responds to every connection
// with a valid SOCKS5 greeting reply (version 5, "no auth" accepted),
// simulating a real SOCKS5 proxy. The caller is responsible for calling the
// returned cleanup.
func listenSocks5Once(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 3)
				if _, err := c.Read(buf); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00})
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenAcceptOnlyOnce starts a TCP listener that accepts connections but
// closes them immediately without writing anything — simulating an open
// port whose service isn't actually SOCKS5 (a misconfigured proxy, a dead
// stub, a captive portal). This is the class of false-positive that the
// older bare-TCP probe couldn't catch.
func listenAcceptOnlyOnce(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// closedPortAddr returns an address nothing is listening on, by opening and
// immediately closing a listener to grab a free port.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestProbeProxySocks5_RealSocks5Server(t *testing.T) {
	addr, cleanup := listenSocks5Once(t)
	defer cleanup()

	if !probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected a real SOCKS5 server at %s to probe true", addr)
	}
}

func TestProbeProxySocks5_ClosedPort(t *testing.T) {
	addr := closedPortAddr(t)

	if probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected closed port %s to probe false", addr)
	}
}

// TestProbeProxySocks5_OpenPortNotSocks5 is a regression test for the gap a
// bare TCP probe leaves: a port that accepts a connection but isn't
// actually running SOCKS5 must be rejected, not treated as reachable.
func TestProbeProxySocks5_OpenPortNotSocks5(t *testing.T) {
	addr, cleanup := listenAcceptOnlyOnce(t)
	defer cleanup()

	if probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected open-but-non-SOCKS5 port %s to probe false", addr)
	}
}

// TestFilterReachableProxyURLLines_KeepsOnlyReachable is the core of the
// fix: free public proxy lists are mostly dead, so the merge step must drop
// unreachable (or non-SOCKS5) entries before they ever get an auth attempt
// (or a slot from the shared auth rate limiter).
func TestFilterReachableProxyURLLines_KeepsOnlyReachable(t *testing.T) {
	socks5Addr, cleanup := listenSocks5Once(t)
	defer cleanup()
	deadAddr := closedPortAddr(t)
	openNonSocks5Addr, cleanup2 := listenAcceptOnlyOnce(t)
	defer cleanup2()

	lines := []string{
		socks5Addr,
		deadAddr,
		openNonSocks5Addr,
		"not a valid line :::",
	}

	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), lines, "", 0)
	// With empty apiHost the probe skips the CONNECT stage, so SOCKS5
	// proxies end up in the socks5Only bucket.
	if len(apiOK) != 0 || len(socks5Only) != 1 || socks5Only[0] != socks5Addr {
		t.Fatalf("expected %q as socks5-only, got apiOK=%v socks5Only=%v", socks5Addr, apiOK, socks5Only)
	}
}

func TestFilterReachableProxyURLLines_EmptyInput(t *testing.T) {
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), nil, "", 0)
	if len(apiOK) != 0 || len(socks5Only) != 0 {
		t.Fatalf("expected empty result for empty input, got apiOK=%v socks5Only=%v", apiOK, socks5Only)
	}
}
