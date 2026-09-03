package main

import (
	"net"
	"strings"
	"testing"
)

func TestIsBlockedSourceIP(t *testing.T) {
	cases := []struct {
		ip     string
		blocked bool
	}{
		{"127.0.0.1", true},   // loopback
		{"::1", true},         // loopback ipv6
		{"10.1.2.3", true},    // RFC1918
		{"172.16.0.1", true},  // RFC1918
		{"192.168.1.1", true}, // RFC1918
		{"169.254.169.254", true}, // link-local (metadata)
		{"8.8.8.8", false},    // public
		{"1.1.1.1", false},    // public
		{".nil.", false},      // invalid -> parsed as public-ish in test, handled by caller
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
	// A source URL pointing at a private IP literal must be rejected.
	err := ssrfVerifyURLHost("http://10.0.0.5/proxies.txt")
	if err == nil {
		t.Fatal("expected private-IP source URL to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "non-global") {
		t.Errorf("unexpected error: %v", err)
	}
	// A public URL passes the host check (dial may still be guarded).
	if err := ssrfVerifyURLHost("http://8.8.8.8/list"); err != nil {
		t.Errorf("public IP should pass host check, got %v", err)
	}
}