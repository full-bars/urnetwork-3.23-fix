package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// listenSocks5TLSOnce starts a TCP listener that completes the SOCKS5
// greeting (no-auth) and answers every CONNECT with REP 0x00 (success),
// then serves a TLS handshake with the provided certificate on the same
// connection — simulating the tunnel to the "API" endpoint behind the
// proxy. If leaf is nil the connection is closed after CONNECT (a plain
// non-TLS fake, like the pre-TLS-probe helpers).
func listenSocks5TLSOnce(t *testing.T, leaf *tls.Certificate) (addr string, cleanup func()) {
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
				if !readSocks5Greeting(c) {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				connectFrame := make([]byte, 10)
				if _, err := c.Read(connectFrame); err != nil {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				if leaf == nil {
					return
				}
				tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
				// A real MITM/transparent server completes the handshake;
				// the client decides whether the presented chain verifies.
				tlsSrv.Handshake()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestProbeProxy_TLSVerifyRejectsMITM is the core hostile-proxy test: the
// fake answers SOCKS5 CONNECT with success (so the old two-stage probe
// would have admitted it), but presents a certificate that does NOT verify
// for the apiHost — the signature of a TLS-intercepting proxy. The probe
// must return probeTLSFailed, never probeAPIReachable.
func TestProbeProxy_TLSVerifyRejectsMITM(t *testing.T) {
	ca := newTestCA(t)
	// Leaf signed by a CA the probe does NOT trust (system roots), for a
	// hostname that is not the apiHost — an interceptor's self-issued cert.
	leaf := ca.issueLeaf(t, []string{"interceptor.example"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "api.beta-test.net", 443)
	if res != probeTLSFailed {
		t.Fatalf("MITM proxy: result = %v, want probeTLSFailed (probeAPIReachable=%v, probeSocks5Only=%v, probeDead=%v)",
			res, probeAPIReachable, probeSocks5Only, probeDead)
	}
}

// TestProbeProxy_TLSVerifyAcceptsTransparent pins the good case: a
// transparent proxy relays the TLS handshake untouched, so the client sees
// the REAL apiHost certificate (here: a leaf issued by the injected test
// CA) and verification succeeds → probeAPIReachable.
func TestProbeProxy_TLSVerifyAcceptsTransparent(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"api.beta-test.net"})

	// Inject the test CA into the probe's trust store for this test only.
	old := proxyProbeTLSClientConfig
	t.Cleanup(func() { proxyProbeTLSClientConfig = old })
	proxyProbeTLSClientConfig = func(serverName string) *tls.Config {
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		return &tls.Config{ServerName: serverName, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "api.beta-test.net", 443)
	if res != probeAPIReachable {
		t.Fatalf("transparent proxy: result = %v, want probeAPIReachable (tlsFailed=%v)", res, probeTLSFailed)
	}
}

// TestProbeProxy_TLSVerifySkipsWhenNoAPIHost pins the cheap-gate behavior:
// probeProxySocks5 calls probeProxy with an empty apiHost (auth-time gate);
// it must NOT attempt a TLS stage (the fake closes after CONNECT — a
// handshake attempt would hang or error) and must still return a socks5
// verdict, not probeTLSFailed.
func TestProbeProxy_TLSVerifySkipsWhenNoAPIHost(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"api.beta-test.net"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	// Empty apiHost → CONNECT stage can't resolve a target, so this returns
	// socks5-only regardless of the TLS fake. The point is it must NOT
	// block or crash trying to handshake.
	res := probeProxy(context.Background(), addr, "", "", "", 0)
	if res == probeTLSFailed || res == probeAPIReachable {
		t.Fatalf("empty apiHost: result = %v, want socks5-only/dead class", res)
	}
}

// TestProbeAndFilterProxyURLLines_TLSFailedNotAdmitted pins the admission
// flow: a line whose proxy passes CONNECT but fails TLS verification must
// NOT land in the apiOK bucket (which becomes the qualified pool), and must
// be surfaced via the socks5-only bucket so the reaper's retry→blacklist
// lifecycle can retire it.
func TestProbeAndFilterProxyURLLines_TLSFailedNotAdmitted(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"interceptor.example"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	lines := []string{addr}
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), lines, "api.beta-test.net", 443)

	if len(apiOK) != 0 {
		t.Fatalf("MITM line admitted to apiOK: %v", apiOK)
	}
	if len(socks5Only) != 1 {
		t.Fatalf("MITM line not surfaced via socks5-only bucket for reaper lifecycle: apiOK=%v socks5Only=%v", apiOK, socks5Only)
	}
}

// issueLeafForHost issues a TLS leaf for either a DNS name or an IP literal
// (the probe's TLS config pins ServerName to the apiHost, which tests may
// set to "127.0.0.1" — x509 verification of an IP ServerName requires an
// IPAddresses SAN, not a DNSName).
func issueLeafForHost(t *testing.T, ca *testCA, host string) tls.Certificate {
	t.Helper()
	if ip := net.ParseIP(host); ip != nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate leaf key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: "test-leaf"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IPAddresses:  []net.IP{ip},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatalf("create leaf cert: %v", err)
		}
		keyBytes, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal leaf key: %v", err)
		}
		tlsCert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}))
		if err != nil {
			t.Fatalf("build tls.Certificate: %v", err)
		}
		return tlsCert
	}
	return ca.issueLeaf(t, []string{host})
}

// withProbeTLSRoot injects the test CA into the probe's TLS trust store for
// the duration of the test, and restores the production config afterwards.
func withProbeTLSRoot(t *testing.T, ca *testCA) {
	t.Helper()
	old := proxyProbeTLSClientConfig
	t.Cleanup(func() { proxyProbeTLSClientConfig = old })
	proxyProbeTLSClientConfig = func(serverName string) *tls.Config {
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		return &tls.Config{ServerName: serverName, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
}

// readSocks5Greeting reads a SOCKS5 greeting robustly: VER(1) NMETHODS(1)
// then NMETHODS method bytes. A credentialed probe offers
// {0x05, 0x02, 0x00, 0x02} (4 bytes); a no-auth probe offers {0x05, 0x01,
// 0x00} (3 bytes). Reading a fixed 3 bytes misaligns the subsequent CONNECT
// frame for credentialed lines — harmless when nothing follows CONNECT, but
// fatal once a TLS handshake reuses the same connection (the leftover byte
// corrupts the ClientHello). Returns true on success.
func readSocks5Greeting(c net.Conn) bool {
	hdr := make([]byte, 2)
	if _, err := c.Read(hdr); err != nil {
		return false
	}
	if hdr[0] != 0x05 || hdr[1] == 0 {
		return false
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := c.Read(methods); err != nil {
		return false
	}
	return true
}

// serveTLSAfterConnect wraps a SOCKS5 connection handler: after the handler
// has written a successful (REP 0x00) CONNECT reply, serve a TLS handshake
// on the same connection with leaf — modeling a transparent proxy whose
// tunnel leads to a real TLS endpoint. The leaf's SAN must cover the
// apiHost the probe verifies against. If rep != 0x00 no TLS is served (the
// connection is refused, matching the underlying handler's reply).
func serveTLSAfterConnect(rep byte, leaf *tls.Certificate, handle func(c net.Conn)) func(c net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		handle(c)
		if rep == 0x00 && leaf != nil {
			tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
			tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
			// A real MITM/transparent server completes the handshake; the
			// client decides whether the presented chain verifies. Errors
			// here are expected (client rejected the cert, or closed early
			// on a refused CONNECT) — the fake's job is just to present.
			tlsSrv.Handshake()
		}
	}
}

// listenSocks5ConnectOnceTLS is listenSocks5ConnectOnce plus a TLS server
// behind every successful (0x00) CONNECT — a faithful transparent-proxy
// fake for tests that must reach probeAPIReachable.
func listenSocks5ConnectOnceTLS(t *testing.T, rep byte, leaf *tls.Certificate) (addr string, cleanup func()) {
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
			go serveTLSAfterConnect(rep, leaf, func(c net.Conn) {
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00}) // greeting: no auth
				connectFrame := make([]byte, 10)
				if _, err := c.Read(connectFrame); err != nil {
					return
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
			})(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSocks5SequencedTLS is listenSocks5Sequenced (Nth CONNECT answered
// with repFor(n), counter at CONNECT-read) plus a TLS server behind
// successful CONNECTs — used by reaper/refresh tests that drive the full
// probeProxy path.
func listenSocks5SequencedTLS(t *testing.T, repFor func(n int) byte, leaf *tls.Certificate) (addr string, connects *atomic.Int64, cleanup func()) {
	t.Helper()
	var n atomic.Int64
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
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00})
				frame := make([]byte, 10)
				if _, err := c.Read(frame); err != nil {
					return
				}
				rep := repFor(int(n.Add(1)))
				c.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				if rep == 0x00 && leaf != nil {
					tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
					tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
					tlsSrv.Handshake()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), &n, func() { ln.Close() }
}

// listenSocks5ApiOKTLS is listenSocks5ApiOK (every CONNECT succeeds) plus a
// TLS server behind each connection — a faithful transparent-proxy fake for
// tests that must reach probeAPIReachable.
func listenSocks5ApiOKTLS(t *testing.T, leaf *tls.Certificate) (addr string, cleanup func()) {
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
			go serveTLSAfterConnect(0x00, leaf, func(c net.Conn) {
				if !readSocks5Greeting(c) {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				connectFrame := make([]byte, 10)
				if _, err := c.Read(connectFrame); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			})(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSocks5SmartTLS is listenSocks5Smart (CONNECT 0x00 only for ok IPs)
// plus a TLS server behind successful CONNECTs.
func listenSocks5SmartTLS(t *testing.T, okIPs map[string]bool, leaf *tls.Certificate) (addr string, cleanup func()) {
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
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00})
				frame := make([]byte, 10)
				if _, err := c.Read(frame); err != nil {
					return
				}
				rep := byte(0x05)
				if len(frame) == 10 && frame[3] == 0x01 && okIPs[net.IP(frame[4:8]).String()] {
					rep = 0x00
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
				if rep == 0x00 && leaf != nil {
					tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
					tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
					tlsSrv.Handshake()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}
