package urnettools

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testPEMCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestPemFingerprint verifies the hub.pin-form fingerprint matches a direct
// SHA256 of the DER certificate.
func TestPemFingerprint(t *testing.T) {
	pemStr := testPEMCert(t)
	fp, err := pemFingerprint(pemStr)
	if err != nil {
		t.Fatalf("pemFingerprint: %v", err)
	}
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) != len("SHA256:")+64 {
		t.Fatalf("bad fingerprint format: %q", fp)
	}
	// Compare against direct sha256 of DER.
	block, _ := pem.Decode([]byte(pemStr))
	sum := sha256.Sum256(block.Bytes)
	want := "SHA256:" + strings.ToLower(hex.EncodeToString(sum[:]))
	if fp != want {
		t.Fatalf("fp = %q, want %q", fp, want)
	}
}

// TestSplitHostPortURL covers host/port extraction including implicit :443.
func TestSplitHostPortURL(t *testing.T) {
	cases := map[string][2]string{
		"https://hub.example.com:8443": {"hub.example.com", "8443"},
		"https://hub.example.com":      {"hub.example.com", "443"},
		"https://10.0.0.5:9443/":       {"10.0.0.5", "9443"},
	}
	for in, want := range cases {
		h, p, err := splitHostPortURL(in)
		if err != nil || h != want[0] || p != want[1] {
			t.Errorf("splitHostPortURL(%q) = %s,%s,%v; want %s,%s", in, h, p, err, want[0], want[1])
		}
	}
}

// TestExtractField checks the hub-binary output field parser.
func TestExtractField(t *testing.T) {
	out := "Token: abc123\nExpires: 2026-01-01T00:00:00Z\nCA fingerprint: SHA256:xyz\n"
	if got := extractField(out, "Token:"); got != "abc123" {
		t.Errorf("Token = %q", got)
	}
	if got := extractField(out, "CA fingerprint:"); got != "SHA256:xyz" {
		t.Errorf("CA fingerprint = %q", got)
	}
	if got := extractField(out, "Nope:"); got != "" {
		t.Errorf("missing field = %q, want empty", got)
	}
}

// TestFetchHubCA parses a real hub JSON response (escaping embedded \n into
// real newlines in the PEM) and the legacy fingerprint fallback.
func TestFetchHubCA(t *testing.T) {
	pemStr := testPEMCert(t)
	escaped := strings.ReplaceAll(pemStr, "\n", `\n`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cert":
			w.Write([]byte(`{"ca_pem":"` + escaped + `","ca_fingerprint":"SHA256:AAA"}`))
		case "/api/ca-cert":
			w.Write([]byte(`{"fingerprint":"SHA256:BBB"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ca, fp, err := fetchHubCA(srv.URL, "")
	if err != nil {
		t.Fatalf("fetchHubCA(cert): %v", err)
	}
	if ca != pemStr {
		t.Fatal("ca_pem was not unescaped to real newlines")
	}
	if fp != "SHA256:AAA" {
		t.Fatalf("fingerprint = %q", fp)
	}

	// /api/ca-cert path with a token returns only the legacy fingerprint.
	_, fp2, err := fetchHubCA(srv.URL, "tok")
	if err != nil {
		t.Fatalf("fetchHubCA(token): %v", err)
	}
	if fp2 != "SHA256:BBB" {
		t.Fatalf("token fingerprint = %q", fp2)
	}
}

// TestURLQueryEscape ensures the onboard token is escaped for a query string.
func TestURLQueryEscape(t *testing.T) {
	if got := urlQueryEscape("ab+/=cd"); got != "ab%2B%2F%3Dcd" {
		t.Fatalf("urlQueryEscape = %q", got)
	}
}
