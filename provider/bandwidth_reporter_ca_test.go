package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCA is a minimal, test-only CA+leaf pair. It doesn't need to match the
// hub's Ed25519/Argon2id derivation scheme — verifyHubChain only cares that
// a real x509 chain exists, so a plain self-signed ECDSA CA is sufficient
// here, and keeps this test file independent of the hub package (provider
// and hub are separate `package main`s and must not import each other).
type testCA struct {
	certDER []byte
	certPEM []byte
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		certDER: der,
		certPEM: pemEncodeCert(der),
		key:     key,
		cert:    cert,
	}
}

func (ca *testCA) issueLeaf(t *testing.T, sans []string) tls.Certificate {
	t.Helper()
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
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	tlsCert, err := tls.X509KeyPair(pemEncodeCert(der), pemEncodeKey(keyBytes))
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	return tlsCert
}

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemEncodeKey(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func TestLoadHubCAPool_MissingFile(t *testing.T) {
	withTempHome(t)

	if _, ok := loadHubCAPool(); ok {
		t.Error("expected ok=false when hub_ca.pem does not exist")
	}
}

func TestLoadHubCAPool_ValidPEM(t *testing.T) {
	home := withTempHome(t)
	ca := newTestCA(t)

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub_ca.pem"), ca.certPEM, 0600); err != nil {
		t.Fatal(err)
	}

	pool, ok := loadHubCAPool()
	if !ok {
		t.Fatal("expected ok=true for a valid PEM file")
	}
	if pool == nil {
		t.Fatal("expected a non-nil pool")
	}
}

func TestLoadHubCAPool_GarbagePEM(t *testing.T) {
	home := withTempHome(t)

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Simulates the onboard.sh base64-corruption failure mode: garbage bytes
	// instead of PEM text.
	if err := os.WriteFile(filepath.Join(dir, "hub_ca.pem"), []byte{0x04, 0x41, 0x88, 0x34, 0x21}, 0600); err != nil {
		t.Fatal(err)
	}

	if _, ok := loadHubCAPool(); ok {
		t.Error("expected ok=false for garbage/non-PEM content")
	}
}

func TestVerifyHubChain_AcceptsChainedLeaf(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"hub.local"})
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	verify := verifyHubChain(pool, "")
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leafCert}}
	if err := verify(cs); err != nil {
		t.Errorf("expected chained leaf to verify, got: %v", err)
	}
}

func TestVerifyHubChain_RejectsForeignCA(t *testing.T) {
	realCA := newTestCA(t)
	foreignCA := newTestCA(t)
	leaf := foreignCA.issueLeaf(t, []string{"hub.local"})
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(realCA.cert)

	verify := verifyHubChain(pool, "")
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leafCert}}
	err = verify(cs)
	if err == nil {
		t.Fatal("expected a leaf signed by a different CA to be rejected")
	}
	if !strings.Contains(err.Error(), "hub CA verification failed") {
		t.Errorf("error = %q, want it to contain the actionable re-onboard message", err.Error())
	}
}

func TestVerifyHubChain_RejectsNoPeerCertificates(t *testing.T) {
	pool := x509.NewCertPool()
	verify := verifyHubChain(pool, "")
	if err := verify(tls.ConnectionState{}); err == nil {
		t.Error("expected an error when there are no peer certificates")
	}
}

// TestNewClientForURL_SystemPoolFallback verifies that an https:// report URL
// with no hub_ca.pem and no legacy hub.pin falls back to the system CA pool
// (works with Cloudflare Tunnel, Caddy+LE, etc.) instead of failing closed.
// On systems where x509.SystemCertPool() is unavailable, it falls through to
// the fail-closed branch.
func TestNewClientForURL_SystemPoolFallback(t *testing.T) {
	withTempHome(t)

	client := newClientForURL("https://hub.example.com")
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatal("expected an *http.Transport with a TLSClientConfig")
	}

	// If SystemCertPool succeeded, the client uses RootCAs and no
	// VerifyConnection callback. If it failed, the fail-closed branch
	// sets a VerifyConnection that always errors.
	if transport.TLSClientConfig.RootCAs != nil {
		// System CA pool fallback — no VerifyConnection, just RootCAs
		if transport.TLSClientConfig.VerifyConnection != nil {
			t.Error("expected no VerifyConnection when using system CA pool fallback")
		}
		if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %v, want TLS 1.2", transport.TLSClientConfig.MinVersion)
		}
	} else {
		// Fail-closed branch on systems without a system CA pool
		verify := transport.TLSClientConfig.VerifyConnection
		if verify == nil {
			t.Fatal("expected a VerifyConnection callback in the fail-closed branch")
		}
		if err := verify(tls.ConnectionState{}); err == nil {
			t.Error("expected the fail-closed VerifyConnection to always return an error")
		}
	}
}

// TestReporterClient_ConnectsToCASignedHub is the end-to-end path: a real
// TLS server presenting a CA-signed leaf, a client trusting only that CA via
// hub_ca.pem, connecting successfully — then, with a different CA installed,
// failing closed instead of silently accepting the mismatched cert.
func TestReporterClient_ConnectsToCASignedHub(t *testing.T) {
	home := withTempHome(t)
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"127.0.0.1"})

	server := httptest.NewUnstartedServer(nil)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	defer server.Close()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "hub_ca.pem")
	if err := os.WriteFile(caPath, ca.certPEM, 0600); err != nil {
		t.Fatal(err)
	}

	client := newClientForURL(server.URL)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("expected the request to succeed against the CA-signed hub, got: %v", err)
	}
	resp.Body.Close()

	// Swap in a different CA — simulates the hub being redeployed with a
	// different password. The provider must fail closed, not fall back to
	// insecure acceptance.
	foreignCA := newTestCA(t)
	if err := os.WriteFile(caPath, foreignCA.certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	client2 := newClientForURL(server.URL)
	_, err = client2.Get(server.URL)
	if err == nil {
		t.Fatal("expected the request to fail once hub_ca.pem no longer matches the server's actual CA")
	}
	if !strings.Contains(err.Error(), "hub CA verification failed") {
		t.Errorf("error = %q, want it to contain the actionable re-onboard message", err.Error())
	}
}

func TestNewClientForURL_LegacyPinDeprecationLoggedOnce(t *testing.T) {
	_ = withTempHome(t)

	pinFile, err := hubPinPath()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Dir(pinFile), 0700)
	_ = os.WriteFile(pinFile, []byte("SHA256:deadbeef\n"), 0600)
	t.Cleanup(func() { os.Remove(pinFile) })

	loggedLegacyPinDeprecation.Store(false)

	_ = newClientForURL("https://hub.example.com")
	if !loggedLegacyPinDeprecation.Load() {
		t.Error("expected deprecation flag set after first legacy-pin client")
	}

	loggedLegacyPinDeprecation.Store(false)
	_ = newClientForURL("https://hub.example.com")
	if !loggedLegacyPinDeprecation.Load() {
		t.Error("expected deprecation flag set again (package-level, reset between calls)")
	}
	loggedLegacyPinDeprecation.Store(false)
}

// --- bootstrapHubCA tests ---

// issueLeafWithIPSAN mirrors testCA.issueLeaf but sets an IP SAN rather than
// a DNS SAN. Standard library TLS hostname verification (used by a plain
// http.Client with RootCAs, unlike this project's own IP-skipping
// verifyHubChain) requires an IP SAN to validate connections made to an IP
// literal like httptest's "127.0.0.1" servers.
func issueLeafWithIPSAN(t *testing.T, ca *testCA, ip string) tls.Certificate {
	t.Helper()
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
		IPAddresses:  []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	tlsCert, err := tls.X509KeyPair(pemEncodeCert(der), pemEncodeKey(keyBytes))
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	return tlsCert
}

func caCertHandler(expectToken, caPEM string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ca_pem": caPEM})
	}
}

func TestBootstrapHubCA_SkipsIfCAAlreadyExists(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "hub_ca.pem")
	if err := os.WriteFile(caPath, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	called := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	bootstrapHubCA(context.Background(), server.URL, "secret-token")

	if called {
		t.Error("expected no request when hub_ca.pem already exists")
	}
	data, _ := os.ReadFile(caPath)
	if string(data) != "existing" {
		t.Error("existing CA file was modified")
	}
}

func TestBootstrapHubCA_NoopWithoutTokenOrHTTPS(t *testing.T) {
	home := withTempHome(t)
	caPath := filepath.Join(home, ".urnetwork", "hub_ca.pem")

	bootstrapHubCA(context.Background(), "https://hub.example.com", "")
	if _, err := os.Stat(caPath); err == nil {
		t.Error("expected no file written with empty token")
	}

	bootstrapHubCA(context.Background(), "http://hub.example.com", "secret-token")
	if _, err := os.Stat(caPath); err == nil {
		t.Error("expected no file written for a plain-HTTP report URL")
	}
}

// TestBootstrapHubCA_FallsBackToInsecureWhenNotPubliclyTrusted exercises the
// realistic case: a self-signed/password-derived-CA hub whose leaf isn't in
// any system trust store, so the verified attempt fails and bootstrapHubCA
// falls back to the unverified fetch to still complete the bootstrap.
func TestBootstrapHubCA_FallsBackToInsecureWhenNotPubliclyTrusted(t *testing.T) {
	home := withTempHome(t)
	ca := newTestCA(t)

	server := httptest.NewTLSServer(caCertHandler("secret-token", string(ca.certPEM)))
	defer server.Close()

	bootstrapHubCA(context.Background(), server.URL, "secret-token")

	caPath := filepath.Join(home, ".urnetwork", "hub_ca.pem")
	data, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("expected hub_ca.pem to be written via insecure fallback: %v", err)
	}
	if !strings.Contains(string(data), "BEGIN CERTIFICATE") {
		t.Errorf("written CA cert doesn't look like a PEM cert: %s", data)
	}
	info, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("hub_ca.pem perms = %v, want 0600", info.Mode().Perm())
	}
}

func TestBootstrapHubCA_RejectsWrongToken(t *testing.T) {
	home := withTempHome(t)
	ca := newTestCA(t)
	server := httptest.NewTLSServer(caCertHandler("correct-token", string(ca.certPEM)))
	defer server.Close()

	bootstrapHubCA(context.Background(), server.URL, "wrong-token")

	caPath := filepath.Join(home, ".urnetwork", "hub_ca.pem")
	if _, err := os.Stat(caPath); err == nil {
		t.Error("expected no CA cert written when the hub rejects the token")
	}
}

func TestBootstrapHubCA_RejectsMissingCAPEMInResponse(t *testing.T) {
	home := withTempHome(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer server.Close()

	bootstrapHubCA(context.Background(), server.URL, "secret-token")

	caPath := filepath.Join(home, ".urnetwork", "hub_ca.pem")
	if _, err := os.Stat(caPath); err == nil {
		t.Error("expected no CA cert written when ca_pem is missing from the response")
	}
}

// TestFetchHubCACert_SucceedsOverAVerifiedConnection simulates the safe path:
// a client that already trusts the hub's CA (e.g. via the system pool, for a
// Caddy+LE/Cloudflare Tunnel hub) fetches ca_pem without ever needing
// InsecureSkipVerify — the token is protected by real certificate validation.
func TestFetchHubCACert_SucceedsOverAVerifiedConnection(t *testing.T) {
	ca := newTestCA(t)
	leaf := issueLeafWithIPSAN(t, ca, "127.0.0.1")

	server := httptest.NewUnstartedServer(caCertHandler("secret-token", string(ca.certPEM)))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)
	verifiedClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}

	apiURL, err := url.JoinPath(server.URL, "/api/ca-cert")
	if err != nil {
		t.Fatal(err)
	}
	caPEM, ok := fetchHubCACert(context.Background(), apiURL, "secret-token", verifiedClient)
	if !ok {
		t.Fatal("expected the verified fetch to succeed against a CA-signed test server")
	}
	if caPEM != string(ca.certPEM) {
		t.Error("returned ca_pem does not match the server's CA cert")
	}
}

func TestWriteHubCACertAtomic_WritesCompleteFileWithCorrectPerms(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "hub_ca.pem")

	if err := writeHubCACertAtomic(caPath, "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----"); err != nil {
		t.Fatalf("writeHubCACertAtomic: %v", err)
	}

	data, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("expected trailing newline appended to written CA cert")
	}
	info, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}

	// No leftover temp file after a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly one file in %s after write, got %d", dir, len(entries))
	}
}
