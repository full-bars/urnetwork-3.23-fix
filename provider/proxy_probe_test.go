package main

import (
	"context"
	"io"
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
	// Liveness barrier: complete one full greeting exchange from the test
	// itself before returning. The accept loop may not be scheduled yet, and
	// a probe that arrives in that window can fail, trip the reaper's 3-fail
	// TLS-verify blacklist, and the just-started fake gets marked dead —
	// flaking every caller that probes right after (the
	// TestFetchAndMergeProxyURLs cache==2 assertions). Dialing through the
	// loop and reading its 0x05 0x00 reply proves both the listener AND the
	// handler goroutine are live.
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
	probe, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("liveness probe dial failed: %v", err)
	}
	if _, err := probe.Write([]byte{0x05, 0x00, 0x00}); err != nil {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe write failed: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(probe, greeting); err != nil {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe: accept loop did not reply to greeting: %v", err)
	}
	if greeting[0] != 0x05 || greeting[1] != 0x00 {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe: unexpected greeting % x", greeting)
	}
	probe.Close()
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

// TestProbeStartupCooldown_ExceedsSystemdRestartSec pins the safety
// invariant documented on probeStartupCooldown: it must stay strictly
// greater than the systemd unit's RestartSec (5s, see
// internal/urnettools/legacy_cmds.go), or a crash-looping process could
// live long enough between restarts to reach the deferred fetch and
// re-trigger the very probe-amplification loop the cooldown exists to
// prevent.
func TestProbeStartupCooldown_ExceedsSystemdRestartSec(t *testing.T) {
	const systemdRestartSec = 5 * time.Second
	if probeStartupCooldown <= systemdRestartSec {
		t.Fatalf("probeStartupCooldown (%v) must be > systemd RestartSec (%v)", probeStartupCooldown, systemdRestartSec)
	}
}

func TestFilterReachableProxyURLLines_EmptyInput(t *testing.T) {
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), nil, "", 0)
	if len(apiOK) != 0 || len(socks5Only) != 0 {
		t.Fatalf("expected empty result for empty input, got apiOK=%v socks5Only=%v", apiOK, socks5Only)
	}
}
