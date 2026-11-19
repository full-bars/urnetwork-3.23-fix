package connect

// A platform transport dialed through a proxy marks its handshake, so the
// platform counts its client at no address of its own
// (transport_egress_proxy.go). One dialed from this host marks nothing.
// Addresses are loopback.

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A socks5 server on loopback that relays each connect request to its target,
// a name resolved here, and counts the connections it relayed.
type testSocksRelay struct {
	listener net.Listener
	relayed  atomic.Int64
}

func newTestSocksRelay(t *testing.T) *testSocksRelay {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &testSocksRelay{listener: listener}
	wg := sync.WaitGroup{}
	connsLock := sync.Mutex{}
	conns := map[net.Conn]bool{}
	track := func(conn net.Conn) bool {
		connsLock.Lock()
		defer connsLock.Unlock()
		if conns == nil {
			conn.Close()
			return false
		}
		conns[conn] = true
		return true
	}
	serve := func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		methods := make([]byte, greeting[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		// no authentication
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil || request[1] != 1 {
			return
		}
		var host string
		switch request[3] {
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom4([4]byte(ip)).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom16([16]byte(ip)).String()
		case 3:
			nameLength := make([]byte, 1)
			if _, err := io.ReadFull(conn, nameLength); err != nil {
				return
			}
			name := make([]byte, nameLength[0])
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			host = string(name)
		default:
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return
		}
		target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), 5*time.Second)
		if err != nil {
			// connection refused
			conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer target.Close()
		if !track(target) {
			return
		}
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		conn.SetDeadline(time.Time{})
		relay.relayed.Add(1)
		done := make(chan struct{}, 2)
		go func() {
			io.Copy(target, conn)
			done <- struct{}{}
		}()
		go func() {
			io.Copy(conn, target)
			done <- struct{}{}
		}()
		<-done
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(conn) {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connsLock.Lock()
		for conn := range conns {
			conn.Close()
		}
		conns = nil
		connsLock.Unlock()
		wg.Wait()
	})
	return relay
}

func TestProviderPlatformProxyHandshake(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		for _, v2 := range []bool{false, true} {
			for _, secure := range []bool{false, true} {
				t.Run(strconv.FormatBool(proxied)+"/"+strconv.FormatBool(v2)+"/"+strconv.FormatBool(secure), func(t *testing.T) {
					headers := make(chan string, 1)
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						select {
						case headers <- r.Header.Get("X-UR-Egress-Proxy"):
						default:
						}
						w.WriteHeader(http.StatusForbidden)
					})
					var server *httptest.Server
					if secure {
						server = httptest.NewTLSServer(handler)
					} else {
						server = httptest.NewServer(handler)
					}
					defer server.Close()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					strategySettings := DefaultClientStrategySettings()
					if secure {
						roots := x509.NewCertPool()
						roots.AddCert(server.Certificate())
						strategySettings.TlsConfig.RootCAs = roots
					}
					var relay *testSocksRelay
					if proxied {
						relay = newTestSocksRelay(t)
						strategySettings.ProxySettings = &ProxySettings{Network: "tcp", Address: relay.listener.Addr().String()}
					}
					strategy := NewClientStrategy(ctx, strategySettings)
					defer strategy.Close()
					settings := DefaultPlatformTransportSettings()
					settings.V2H1Auth = v2
					transport := NewPlatformTransportWithTargetMode(ctx, strategy, NewRouteManager(ctx, "test"), "ws"+strings.TrimPrefix(server.URL, "http"), &ClientAuth{ByJwt: "jwt", InstanceId: NewId(), AppVersion: "test"}, TransportModeH1, settings)
					defer transport.Close()
					select {
					case got := <-headers:
						expected := ""
						if proxied {
							expected = "1"
							if relay.relayed.Load() == 0 {
								t.Fatal("handshake bypassed SOCKS relay")
							}
						}
						if got != expected {
							t.Fatalf("proxy marker = %q, want %q", got, expected)
						}
					case <-time.After(15 * time.Second):
						t.Fatal("platform handshake did not arrive")
					}
				})
			}
		}
	}
}
