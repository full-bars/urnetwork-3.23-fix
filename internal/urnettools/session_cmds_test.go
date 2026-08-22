package urnettools

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testPass = "correct horse battery staple"

// TestSessionRoundTrip encrypts a set of identity files and decrypts them
// back, asserting the openssl Salted__ header and that content survives.
func TestSessionRoundTrip(t *testing.T) {
	files := map[string][]byte{
		"jwt":               []byte("header.payload.sig"),
		".client_jwts.json": []byte(`{"entries":1}`),
		"proxy":             []byte("p1:p2"),
	}
	// Pin the salt so the output is deterministic and verifiable.
	sessionRandSalt = func() ([]byte, error) { return []byte("01234567"), nil }
	bundle, err := tarAndEncrypt(files, testPass)
	if err != nil {
		t.Fatalf("tarAndEncrypt: %v", err)
	}
	if !strings.HasPrefix(string(bundle), "Salted__") {
		t.Fatalf("bundle missing openssl Salted__ header")
	}
	got, err := decryptUntar(string(bundle), testPass)
	if err != nil {
		t.Fatalf("decryptUntar: %v", err)
	}
	for name, want := range files {
		if string(got[name]) != string(want) {
			t.Errorf("file %q = %q, want %q", name, got[name], want)
		}
	}
}

// TestSessionRoundTripWrongPass ensures a wrong passphrase is rejected, not
// silently producing garbage.
func TestSessionRoundTripWrongPass(t *testing.T) {
	files := map[string][]byte{"jwt": []byte("header.payload.sig")}
	sessionRandSalt = func() ([]byte, error) { return []byte("01234567"), nil }
	bundle, err := tarAndEncrypt(files, testPass)
	if err != nil {
		t.Fatalf("tarAndEncrypt: %v", err)
	}
	if _, err := decryptUntar(string(bundle), "wrong-pass"); err == nil {
		t.Fatalf("expected error decrypting with wrong passphrase, got nil")
	}
}

// TestSessionNetworkIDFromJWT builds a valid JWT payload and checks that
// sessionNetworkID extracts the network_id claim.
func TestSessionNetworkIDFromJWT(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pl, _ := json.Marshal(map[string]string{
		"network_id":   "net-123",
		"network_name": "testnet",
	})
	payload := base64.RawURLEncoding.EncodeToString(pl)
	jwt := header + "." + payload + ".sig"
	files := map[string][]byte{"jwt": []byte(jwt)}
	if got := sessionNetworkID(files); got != "net-123" {
		t.Fatalf("sessionNetworkID = %q, want net-123", got)
	}
	if !sessionHasJWT(files) {
		t.Fatalf("sessionHasJWT should be true with a jwt present")
	}
}

// TestSessionHelpRouting checks `session --help` prints help and returns nil
// without a live provider (help-never-executes).
func TestSessionHelpRouting(t *testing.T) {
	if err := Run([]string{"session", "--help"}); err != nil {
		t.Fatalf("Run([session --help]) = %v, want nil", err)
	}
	if err := Run([]string{"session"}); err == nil {
		t.Fatalf("Run([session]) should error (missing action)")
	}
}
