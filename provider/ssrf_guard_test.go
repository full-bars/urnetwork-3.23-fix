package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestIsBlockedSourceIP(t *testing.T) {
	ssrfAllowLoopback.Store(false)
	defer ssrfAllowLoopback.Store(true)

	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},       // loopback
		{"::1", true},             // loopback ipv6
		{"10.1.2.3", true},        // RFC1918
		{"172.16.0.1", true},      // RFC1918
		{"192.168.1.1", true},     // RFC1918
		{"169.254.169.254", true}, // link-local (metadata)
		{"8.8.8.8", false},        // public
		{"1.1.1.1", false},        // public
		{".nil.", false},          // invalid -> parsed as public-ish in test, handled by caller
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		got := isBlockedSourceIP(ip)
		if got != c.blocked {
			// only assert for parseable IPs; invalid is caller's concern
			if ip != nil {
				t.Errorf("isBlockedSourceIP(%s) = %v, want %v", c.ip, got, c.blocked)
			}
		}
	}
}

func TestSSRFVerifyURLHost_BlocksPrivate(t *testing.T) {
	ssrfAllowLoopback.Store(false)
	defer ssrfAllowLoopback.Store(true)

	// A source URL pointing at a private IP literal must be rejected.
	err := ssrfVerifyURLHost("http://10.0.0.5/proxies.txt")
	if err == nil {
		t.Fatal("expected private-IP source URL to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "non-global") {
		t.Errorf("unexpected error: %v", err)
	}
	// Loopback is rejected by default.
	if err := ssrfVerifyURLHost("http://127.0.0.1/proxies.txt"); err == nil {
		t.Fatal("expected loopback source URL to be rejected by default, got nil")
	}
	// A public URL passes the host check (dial may still be guarded).
	if err := ssrfVerifyURLHost("http://8.8.8.8/list"); err != nil {
		t.Errorf("public IP should pass host check, got %v", err)
	}
}

func TestSSRF_AllowLoopbackForTesting(t *testing.T) {
	ssrfAllowLoopback.Store(true)

	// Loopback is allowed when toggle is on.
	if err := ssrfVerifyURLHost("http://127.0.0.1/proxies.txt"); err != nil {
		t.Errorf("expected loopback to be allowed when toggle is on, got %v", err)
	}
	// RFC1918 private IPs are STILL rejected even when loopback is allowed.
	if err := ssrfVerifyURLHost("http://10.0.0.5/proxies.txt"); err == nil {
		t.Fatal("expected private-IP source URL to be rejected even with loopback allowed")
	}
	if err := ssrfVerifyURLHost("http://192.168.1.1/proxies.txt"); err == nil {
		t.Fatal("expected 192.168 private-IP to be rejected even with loopback allowed")
	}
	if err := ssrfVerifyURLHost("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatal("expected link-local to be rejected even with loopback allowed")
	}
}

// TestSSRFDialContext_BlocksHostnameResolvingToPrivate is a regression test:
// ssrfDialContext previously only validated addr when it was already an IP
// literal (net.ParseIP(host) != nil). For the common case — a hostname
// source URL — that check was always skipped, and net.Dialer resolved and
// dialed the hostname with zero SSRF validation. "localhost" resolves to a
// loopback address, so with ssrfAllowLoopback off the dial must be refused
// before any connection is attempted.
func TestSSRFDialContext_BlocksHostnameResolvingToPrivate(t *testing.T) {
	ssrfAllowLoopback.Store(false)
	defer ssrfAllowLoopback.Store(true)

	conn, err := ssrfDialContext(context.Background(), "tcp", "localhost:80")
	if err == nil {
		conn.Close()
		t.Fatal("expected dial to hostname resolving to loopback to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("expected SSRF guard error, got: %v", err)
	}
}
