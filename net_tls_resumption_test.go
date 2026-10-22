package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClientTlsConfigGivesEachPathItsOwnCache(t *testing.T) {
	base := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}
	baseCache := tls.NewLRUClientSessionCache(4)
	base.ClientSessionCache = baseCache

	a := newClientTlsConfig(base, nil)
	b := newClientTlsConfig(base, nil)
	if a.ClientSessionCache == nil || b.ClientSessionCache == nil {
		t.Fatal("every path needs a session cache")
	}
	if a.ClientSessionCache == b.ClientSessionCache {
		t.Fatal("two paths must not share a cache: a ticket would link them")
	}
	if a.ClientSessionCache == baseCache {
		t.Fatal("a caller-supplied cache must be replaced, not inherited")
	}
	if base.ClientSessionCache != baseCache {
		t.Fatal("the base config must not be mutated")
	}
	if len(a.NextProtos) != 1 || a.NextProtos[0] != "h2" {
		t.Fatalf("a nil protocol list keeps the base's: %v", a.NextProtos)
	}
	c := newClientTlsConfig(base, []string{"http/1.1"})
	if len(c.NextProtos) != 1 || c.NextProtos[0] != "http/1.1" || base.NextProtos[0] != "h2" {
		t.Fatalf("a protocol list replaces only the copy: %v / base %v", c.NextProtos, base.NextProtos)
	}
	if d := newClientTlsConfig(nil, nil); d == nil || d.ClientSessionCache == nil {
		t.Fatal("a nil base must still yield a usable config with a cache")
	}
}

func resilientTestSettings(t *testing.T) (*httptest.Server, *ConnectSettings) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	settings := DefaultConnectSettings()
	settings.TlsConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return server, settings
}

// tlsConnectionStateConn is the part of a dialed tls connection these tests
// read: both crypto/tls's *tls.Conn and the Chrome hello's *chromeTlsConn
// answer ConnectionState in crypto/tls's type (net_tls_hello.go).
type tlsConnectionStateConn interface {
	net.Conn
	ConnectionState() tls.ConnectionState
}

// dialResumed dials once and reports whether the handshake resumed a session.
func dialResumed(t *testing.T, dial DialTlsContextFunction, addr string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	tlsConn, ok := conn.(tlsConnectionStateConn)
	if !ok {
		t.Fatalf("expected a tls connection, got %T", conn)
	}
	// a TLS 1.3 session ticket arrives after the handshake; read to take it in
	tlsConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	tlsConn.Read(make([]byte, 1))
	return tlsConn.ConnectionState().DidResume
}

// A re-dial through the same resilient dial path resumes, which is what skips
// certificate chain verification; counters see one full handshake then resumes.
func TestResilientDialResumesOnRedial(t *testing.T) {
	server, settings := resilientTestSettings(t)
	dial := NewResilientDialTlsContext(settings, false, false)
	addr := server.Listener.Addr().String()

	fullBefore := streamMetrics.tlsHandshakeTotal(tlsResultFull)
	resumedBefore := streamMetrics.tlsHandshakeTotal(tlsResultResumed)

	got := []bool{dialResumed(t, dial, addr), dialResumed(t, dial, addr), dialResumed(t, dial, addr)}
	if got[0] || !got[1] || !got[2] {
		t.Fatalf("want full, resumed, resumed; got %v", got)
	}
	if d := streamMetrics.tlsHandshakeTotal(tlsResultFull) - fullBefore; d != 1 {
		t.Fatalf("one full handshake expected, counted %d", d)
	}
	if d := streamMetrics.tlsHandshakeTotal(tlsResultResumed) - resumedBefore; d != 2 {
		t.Fatalf("two resumed handshakes expected, counted %d", d)
	}
}

// Two dial paths never exchange tickets, so a server cannot link them.
func TestResilientDialPathsDoNotShareTickets(t *testing.T) {
	server, settings := resilientTestSettings(t)
	pathA := NewResilientDialTlsContext(settings, false, false)
	pathB := NewResilientDialTlsContext(settings, false, false)
	addr := server.Listener.Addr().String()

	if dialResumed(t, pathA, addr) {
		t.Fatal("path A's first dial cannot resume")
	}
	if !dialResumed(t, pathA, addr) {
		t.Fatal("path A's second dial must resume")
	}
	if dialResumed(t, pathB, addr) {
		t.Fatal("path B must not resume a ticket that path A earned")
	}
}

// The verification still happens on the first handshake: an untrusted server
// is refused, and nothing is cached for it.
func TestResilientDialStillVerifiesTheServerCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	settings := DefaultConnectSettings()
	settings.TlsConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	dial := NewResilientDialTlsContext(settings, false, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failedBefore := streamMetrics.tlsHandshakeTotal(tlsResultFailed)
	if conn, err := dial(ctx, "tcp", server.Listener.Addr().String()); err == nil {
		conn.Close()
		t.Fatal("a server outside the pinned roots must be refused")
	}
	if streamMetrics.tlsHandshakeTotal(tlsResultFailed)-failedBefore != 1 {
		t.Fatal("the refused handshake must be counted as failed")
	}
}

// Every default config shares one parsed pinned pool; PinnedCertPool still
// hands a caller a fresh one it may modify.
func TestDefaultTlsConfigSharesOnePinnedPool(t *testing.T) {
	a, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	b, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if a.RootCAs != b.RootCAs {
		t.Fatal("default configs must share the parsed pinned pool")
	}
	fresh, err := PinnedCertPool()
	if err != nil {
		t.Fatal(err)
	}
	if fresh == a.RootCAs {
		t.Fatal("PinnedCertPool must return a fresh pool a caller can modify")
	}
	if !fresh.Equal(a.RootCAs) {
		t.Fatal("the shared pool must hold the same roots")
	}
}
