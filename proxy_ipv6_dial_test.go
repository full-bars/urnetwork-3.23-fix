package connect

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// testSocks5 is a minimal no-auth SOCKS5 CONNECT server. Like a dual stack
// proxy it accepts IPv4, IPv6 and domain targets, and resolves a domain with
// the system resolver in both families.
func testSocks5(t *testing.T) (addr string, seen chan string) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	seen = make(chan string, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				io.ReadFull(c, buf[:int(buf[1])])
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				var host string
				kind := "domain"
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4])
					host, kind = net.IP(append([]byte(nil), buf[:4]...)).String(), "ipv4"
				case 4:
					io.ReadFull(c, buf[:16])
					host, kind = net.IP(append([]byte(nil), buf[:16]...)).String(), "ipv6"
				case 3:
					io.ReadFull(c, buf[:1])
					n := int(buf[0])
					io.ReadFull(c, buf[:n])
					host = string(buf[:n])
				}
				io.ReadFull(c, buf[:2])
				port := int(buf[0])<<8 | int(buf[1])
				seen <- kind
				up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
				if err != nil {
					c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return l.Addr().String(), seen
}

func echoOn(t *testing.T, network, address string) int {
	t.Helper()
	l, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("cannot listen on %s %s: %v", network, address, err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); c.Write([]byte("hi")) }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// TestProxyDialIPv6Matrix dials four kinds of target through a SOCKS5 server
// that, like a dual stack proxy, handles IPv4, IPv6 and name targets. It runs
// each in both proxy_target_family modes. An IPv6 literal, an IPv4 literal and a
// name with only an IPv6 address must connect in both modes. A name with both
// families whose service listens on IPv6 only fails in the default v4 mode,
// because the A-only lookup picks the IPv4 address. That case is recorded, not
// required, so the day the default changes the test says so.
func TestProxyDialIPv6Matrix(t *testing.T) {
	proxyAddr, seen := testSocks5(t)
	v6port := echoOn(t, "tcp6", "[::1]:0")
	v4port := echoOn(t, "tcp4", "127.0.0.1:0")
	ps := &ProxySettings{Index: 1, Network: "tcp", Address: proxyAddr}
	prev := ProxyTargetFamily()
	t.Cleanup(func() { SetProxyTargetFamily(prev) })

	cases := []struct {
		name, target string
		known        bool
	}{
		{"IPv6 literal", net.JoinHostPort("::1", strconv.Itoa(v6port)), false},
		{"IPv4 literal", net.JoinHostPort("127.0.0.1", strconv.Itoa(v4port)), false},
		{"name with only an IPv6 address (ip6-localhost)", net.JoinHostPort("ip6-localhost", strconv.Itoa(v6port)), false},
		{"name with both families, service on IPv6 only (localhost)", net.JoinHostPort("localhost", strconv.Itoa(v6port)), true},
	}
	// the two name cases need the host's resolver to know these names
	if addrs, err := net.LookupHost("ip6-localhost"); err != nil || len(addrs) == 0 {
		cases = append(cases[:2], cases[3:]...)
	}
	if addrs, err := net.LookupHost("localhost"); err != nil || len(addrs) < 2 {
		cases = cases[:len(cases)-1]
	}
	for _, mode := range []ProxyTargetFamilyMode{ProxyTargetFamilyV4, ProxyTargetFamilyProxy} {
		SetProxyTargetFamily(mode)
		for _, c := range cases {
			dial := ps.NewDialContext(context.Background(), proxy.Direct)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			conn, err := dial(ctx, "tcp", c.target)
			cancel()
			got := "FAIL"
			if err == nil {
				b := make([]byte, 2)
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, rerr := io.ReadFull(conn, b); rerr == nil && string(b) == "hi" {
					got = "ok"
				}
				conn.Close()
			}
			kind := "-"
			select {
			case kind = <-seen:
			default:
			}
			if got != "ok" && !(mode == ProxyTargetFamilyV4 && c.known) {
				t.Errorf("mode=%s %s: %s (proxy was sent %s): %v", mode, c.name, got, kind, err)
			}
			t.Logf("mode=%-5s | %-58s | %-4s | proxy was sent: %s | err=%v", mode, c.name, got, kind, err)
		}
	}
}
