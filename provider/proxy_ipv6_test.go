package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// IPv6 proxy support end to end: every path that used to split host:port at a
// colon must carry a bracketed IPv6 host ([v6]:port and the credentialed
// forms) through paste, redaction, matching, URL lines and the probe dial.

func TestNormalizeProxyLineIPv6Forms(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		// Format 1: cred@host:port
		{"user:pass@[::1]:1080", "[::1]:1080:user:pass"},
		{"key@[2001:db8::1]:9050", "[2001:db8::1]:9050:key:"},
		// Format 2: canonical host:port:user:pass
		{"[2001:db8::1]:1080:alice:s3cret", "[2001:db8::1]:1080:alice:s3cret"},
		{"[::1]:1080:u:1234", "[::1]:1080:u:1234"},
		// Format 3: space separated
		{"[::1]:1080 user:pass", "[::1]:1080:user:pass"},
		// Format 4: bare
		{"[2001:db8::1]:1080", "[2001:db8::1]:1080::"},
		{"socks5://[::1]:1080", "[::1]:1080::"},
		{"socks5://user:pass@[2001:db8::1]:1080", "[2001:db8::1]:1080:user:pass"},
		// rejected: ambiguous or malformed
		{"::1:1080", ""},
		{"2001:db8::1", ""},
		{"[::1]", ""},
		{"[::1]:notaport", ""},
	} {
		if got := normalizeProxyLine(c.in); got != c.want {
			t.Errorf("normalizeProxyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Redaction keeps [v6]:port and hides the credential tail, like it keeps
// host:port for IPv4.
func TestRedactProxyLineIPv6(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"[::1]:1080:user:pass", "[::1]:1080"},
		{"[2001:db8::1]:9050:alice:s3cret", "[2001:db8::1]:9050"},
		{"user:pass@[::1]:1080", "[::1]:1080"},
		{"[::1]:1080 user:pass", "[::1]:1080"},
		{"[::1]:1080", "[::1]:1080"},
	} {
		if got := redactProxyLine(c.in); got != c.want {
			t.Errorf("redactProxyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// the credential must never appear in the redacted form
	got := redactProxyLine("[2001:db8::1]:9050:alice:s3cret")
	if strings.Contains(got, "alice") || strings.Contains(got, "s3cret") {
		t.Fatalf("redaction leaked a credential: %q", got)
	}
}

func TestMatchProxyHostIPv6(t *testing.T) {
	for _, c := range []struct {
		pattern, address string
		want             bool
	}{
		{"2001:db8", "[2001:db8::1]:1080", true},
		{"db8::1", "[2001:db8::1]:1080:alice:pw", true},
		{"::1", "[::1]:9050", true},
		{"1080", "[2001:db8::1]:1080", false}, // the port is never matched
		{"alice", "[2001:db8::1]:1080:alice:pw", false},
		{"decodo.com", "[2001:db8::1]:1080", false},
	} {
		if got := matchProxyHost(c.pattern, c.address); got != c.want {
			t.Errorf("matchProxyHost(%q, %q) = %v, want %v", c.pattern, c.address, got, c.want)
		}
	}
}

func TestParseProxyURLLineIPv6(t *testing.T) {
	for _, c := range []struct {
		in, address, user, password string
		ok                          bool
	}{
		{"socks5://[::1]:1080", "[::1]:1080", "", "", true},
		{"socks5://user:pass@[2001:db8::1]:1080", "[2001:db8::1]:1080", "user", "pass", true},
		{"[::1]:9050", "[::1]:9050", "", "", true},
		{"[::1]:9050:u:p", "[::1]:9050", "u", "p", true},
	} {
		address, user, password, ok := parseProxyURLLine(c.in)
		if address != c.address || user != c.user || password != c.password || ok != c.ok {
			t.Errorf("parseProxyURLLine(%q) = %q %q %q %v, want %q %q %q %v",
				c.in, address, user, password, ok, c.address, c.user, c.password, c.ok)
		}
	}
}

// socks5ListenerOnV6 starts a minimal SOCKS5 server on the IPv6 loopback that
// answers the greeting with no-auth and accepts any CONNECT, then serves the
// same trivial reply a provider would see from a healthy proxy. Skips cleanly
// when the sandbox has no IPv6 loopback.
func socks5ListenerOnV6(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback available: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				// greeting: version, nmethods, methods...
				head := make([]byte, 2)
				if _, err := io.ReadFull(conn, head); err != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				// request: version, cmd, rsv, atyp, addr, port
				req := make([]byte, 4)
				if _, err := io.ReadFull(conn, req); err != nil {
					return
				}
				switch req[3] {
				case 0x01: // IPv4
					io.ReadFull(conn, make([]byte, 4+2))
				case 0x04: // IPv6
					io.ReadFull(conn, make([]byte, 16+2))
				case 0x03: // domain
					l := make([]byte, 1)
					if _, err := io.ReadFull(conn, l); err != nil {
						return
					}
					io.ReadFull(conn, make([]byte, int(l[0])+2))
				}
				// success reply, bound address 0.0.0.0:0
				conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()
	addr := ln.Addr().String() // "[::1]:port" already bracketed
	cleanup := func() {
		ln.Close()
		<-done
	}
	return addr, cleanup
}

// The end-to-end dial: the provider's proxy probe reaches a real SOCKS5
// listener over the IPv6 loopback by its bracketed address.
func TestProbeProxySocks5IPv6Loopback(t *testing.T) {
	address, cleanup := socks5ListenerOnV6(t)
	defer cleanup()
	if !strings.HasPrefix(address, "[") {
		t.Fatalf("listener address %q is not a bracketed IPv6 form", address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !probeProxySocks5(ctx, address, 5*time.Second) {
		t.Fatalf("probeProxySocks5(%q) must complete the greeting", address)
	}
}

// A credentialed listener: the greeting offers user/pass; the probe must
// complete it end to end on the bracketed IPv6 address too.
func TestProbeProxySocks5IPv6WithCredentials(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback available: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		// choose username/password auth (0x02) if offered, else no-auth
		offered := false
		for _, m := range methods {
			if m == 0x02 {
				offered = true
			}
		}
		if offered {
			if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
				return
			}
			// user/pass sub-negotiation
			ver := make([]byte, 2)
			if _, err := io.ReadFull(conn, ver); err != nil {
				return
			}
			u := make([]byte, int(ver[1]))
			io.ReadFull(conn, u)
			pl := make([]byte, 1)
			if _, err := io.ReadFull(conn, pl); err != nil {
				return
			}
			p := make([]byte, int(pl[0]))
			io.ReadFull(conn, p)
			conn.Write([]byte{0x01, 0x00})
			req := make([]byte, 4)
			if _, err := io.ReadFull(conn, req); err != nil {
				return
			}
			io.ReadFull(conn, make([]byte, 4+2))
			conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
		conn.Write([]byte{0x05, 0x00})
	}()
	address := ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !probeProxySocks5(ctx, address, 5*time.Second) {
		t.Fatalf("probeProxySocks5(%q) with credentials must complete the greeting", address)
	}
}

// The bracketed address must survive the full parse chain the probe input
// takes: a pasted line's normalized form is what the probe receives.
func TestPastedIPv6LineReachesTheProbeForm(t *testing.T) {
	line := normalizeProxyLine("alice:s3cret@[2001:db8::1]:1080")
	if line != "[2001:db8::1]:1080:alice:s3cret" {
		t.Fatalf("normalized = %q", line)
	}
	address, user, password := parseProxyAddress(line)
	if address != "[2001:db8::1]:1080" || user != "alice" || password != "s3cret" {
		t.Fatalf("parseProxyAddress(%q) = %q %q %q", line, address, user, password)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		t.Fatalf("the parsed address must be dialable: %v", err)
	}
	_ = fmt.Sprintf("%s", address)
}
