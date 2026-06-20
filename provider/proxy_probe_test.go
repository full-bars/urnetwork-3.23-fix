package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// listenOnce starts a TCP listener bound to an ephemeral port, accepting (and
// immediately closing) connections in the background, and returns its
// address. The caller is responsible for calling the returned cleanup.
func listenOnce(t *testing.T) (addr string, cleanup func()) {
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

func TestProbeProxyReachable_OpenPort(t *testing.T) {
	addr, cleanup := listenOnce(t)
	defer cleanup()

	if !probeProxyReachable(context.Background(), addr, time.Second) {
		t.Errorf("expected reachable address %s to probe true", addr)
	}
}

func TestProbeProxyReachable_ClosedPort(t *testing.T) {
	addr := closedPortAddr(t)

	if probeProxyReachable(context.Background(), addr, time.Second) {
		t.Errorf("expected closed port %s to probe false", addr)
	}
}

// TestFilterReachableProxyURLLines_KeepsOnlyReachable is the core of the
// fix: free public proxy lists are mostly dead, so the merge step must drop
// unreachable entries before they ever get an auth attempt (or a slot from
// the shared auth rate limiter).
func TestFilterReachableProxyURLLines_KeepsOnlyReachable(t *testing.T) {
	openAddr, cleanup := listenOnce(t)
	defer cleanup()
	deadAddr := closedPortAddr(t)

	lines := []string{
		openAddr,
		deadAddr,
		"not a valid line :::",
	}

	got := filterReachableProxyURLLines(context.Background(), lines)
	if len(got) != 1 || got[0] != openAddr {
		t.Fatalf("expected only %q to survive, got %v", openAddr, got)
	}
}

func TestFilterReachableProxyURLLines_EmptyInput(t *testing.T) {
	got := filterReachableProxyURLLines(context.Background(), nil)
	if len(got) != 0 {
		t.Fatalf("expected empty result for empty input, got %v", got)
	}
}
