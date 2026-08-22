package urnettools

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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

// TestStageSessionFilesSameAccount verifies the load safety logic: staging is
// refused for a different network_id without force, accepted with force, and
// always backs up + stages + marks pending.
func TestStageSessionFilesSameAccount(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	os.WriteFile(filepath.Join(dir, "jwt"), []byte(jwtFor("net-a")), 0o600)

	filesA := map[string][]byte{"jwt": []byte(jwtFor("net-a")), "proxy": []byte("p")}
	if _, err := stageSessionFiles(p, filesA, false); err != nil {
		t.Fatalf("same-account stage should succeed, got %v", err)
	}
	// .session-pending flag written means "load requested"
	if _, err := os.Stat(filepath.Join(dir, ".session-pending")); err != nil {
		t.Fatalf(".session-pending flag missing: %v", err)
	}
	// staged jwt present
	staged, err := os.ReadFile(filepath.Join(dir, ".session-staging/jwt"))
	if err != nil || string(staged) != jwtFor("net-a") {
		t.Fatalf("staged jwt wrong: %v %s", err, staged)
	}

	// Different account, no force: must be refused, nothing staged.
	filesB := map[string][]byte{"jwt": []byte(jwtFor("net-b"))}
	if _, err := stageSessionFiles(p, filesB, false); err == nil {
		t.Fatal("different-account without -f must be refused")
	}
	// state must still be the A staged content (B was rejected before staging)
	staged, _ = os.ReadFile(filepath.Join(dir, ".session-staging/jwt"))
	if string(staged) != jwtFor("net-a") {
		t.Fatalf("rejected load must not overwrite staged session, got %s", staged)
	}

	// With force: different account staged.
	if _, err := stageSessionFiles(p, filesB, true); err != nil {
		t.Fatalf("different-account with -f must succeed, got %v", err)
	}
	staged, _ = os.ReadFile(filepath.Join(dir, ".session-staging/jwt"))
	if string(staged) != jwtFor("net-b") {
		t.Fatalf("forced load must stage net-b, got %s", staged)
	}
}

// jwtFor builds a minimal JWT whose payload carries the given network_id.
func jwtFor(netID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pl, _ := json.Marshal(map[string]string{"network_id": netID, "network_name": "testnet"})
	return header + "." + base64.RawURLEncoding.EncodeToString(pl) + ".sig"
}
