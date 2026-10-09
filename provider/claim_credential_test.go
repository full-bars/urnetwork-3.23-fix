package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
)

func claimTestJwt(t *testing.T, claims gojwt.MapClaims) string {
	t.Helper()
	tok := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims)
	s, err := tok.SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("URNETWORK_STATE_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func claimTestStore(t *testing.T, entries map[string]string) {
	t.Helper()
	path, err := providerStatePath(".client_jwts.json")
	if err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)
	for key, jwt := range entries {
		if err := store.Put(key, clientJWTEntry{ByClientJWT: jwt, ClientID: "c-" + key, MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveClaimCredentialRefusesNoSource(t *testing.T) {
	claimTestHome(t)
	_, err := resolveClaimCredential(docopt.Opts{})
	if err == nil || !strings.Contains(err.Error(), "--store-client") {
		t.Fatalf("no source must refuse and name the options, got %v", err)
	}
}

func TestResolveClaimCredentialRefusesTwoSources(t *testing.T) {
	claimTestHome(t)
	_, err := resolveClaimCredential(docopt.Opts{"--store-client": "direct", "--provider-jwt": "/x"})
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("two sources must refuse, got %v", err)
	}
}

func TestResolveClaimCredentialStoreClient(t *testing.T) {
	claimTestHome(t)
	good := claimTestJwt(t, gojwt.MapClaims{"client_id": "abc", "exp": time.Now().Add(time.Hour).Unix()})
	claimTestStore(t, map[string]string{"direct": good})

	cred, err := resolveClaimCredential(docopt.Opts{"--store-client": "direct"})
	if err != nil || cred.ByJwt != good || cred.LegacyColdkey != "" {
		t.Fatalf("store client: cred=%+v err=%v", cred, err)
	}
	if strings.Contains(cred.Source, good) {
		t.Fatal("the source description must never contain the token")
	}

	_, err = resolveClaimCredential(docopt.Opts{"--store-client": "missing:1080"})
	if err == nil || !strings.Contains(err.Error(), "no client") {
		t.Fatalf("a missing store key must say so, got %v", err)
	}
}

func TestResolveClaimCredentialRejectsNetworkAndExpiredTokens(t *testing.T) {
	claimTestHome(t)
	network := claimTestJwt(t, gojwt.MapClaims{"network_id": "n", "exp": time.Now().Add(time.Hour).Unix()})
	expired := claimTestJwt(t, gojwt.MapClaims{"client_id": "abc", "exp": time.Now().Add(-time.Hour).Unix()})
	claimTestStore(t, map[string]string{"net": network, "old": expired})

	if _, err := resolveClaimCredential(docopt.Opts{"--store-client": "net"}); err == nil || !strings.Contains(err.Error(), "does not name a client") {
		t.Fatalf("a network token must not be accepted as a client token, got %v", err)
	}
	if _, err := resolveClaimCredential(docopt.Opts{"--store-client": "old"}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired token must be refused, got %v", err)
	}
}

func TestResolveClaimCredentialProviderJwtFile(t *testing.T) {
	home := claimTestHome(t)
	good := claimTestJwt(t, gojwt.MapClaims{"client_id": "abc", "exp": time.Now().Add(time.Hour).Unix()})
	path := filepath.Join(home, "client.jwt")
	if err := os.WriteFile(path, []byte(good+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, err := resolveClaimCredential(docopt.Opts{"--provider-jwt": path})
	if err != nil || cred.ByJwt != good {
		t.Fatalf("file credential: cred=%+v err=%v", cred, err)
	}
}

func TestResolveClaimCredentialLegacyColdkey(t *testing.T) {
	home := claimTestHome(t)
	network := claimTestJwt(t, gojwt.MapClaims{"network_id": "n"})
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "jwt"), []byte(network), 0o600); err != nil {
		t.Fatal(err)
	}
	const coldkey = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

	cred, err := resolveClaimCredential(docopt.Opts{"--legacy-coldkey": coldkey})
	if err != nil || cred.ByJwt != network || cred.LegacyColdkey != coldkey {
		t.Fatalf("legacy: cred=%+v err=%v", cred, err)
	}
	if _, err := resolveClaimCredential(docopt.Opts{"--legacy-coldkey": "not-an-address"}); err == nil {
		t.Fatal("an invalid coldkey must be refused before any request")
	}
}

func TestResolveClaimCredentialLegacyColdkeyRefusesExpiredNetworkToken(t *testing.T) {
	home := claimTestHome(t)
	expired := claimTestJwt(t, gojwt.MapClaims{"network_id": "n", "exp": time.Now().Add(-time.Hour).Unix()})
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "jwt"), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveClaimCredential(docopt.Opts{"--legacy-coldkey": "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired network token must be refused locally, got %v", err)
	}
}

func TestResolveClaimCredentialStoreClientMatchesCredentialedAddress(t *testing.T) {
	claimTestHome(t)
	good := claimTestJwt(t, gojwt.MapClaims{"client_id": "abc", "exp": time.Now().Add(time.Hour).Unix()})
	other := claimTestJwt(t, gojwt.MapClaims{"client_id": "def", "exp": time.Now().Add(time.Hour).Unix()})
	claimTestStore(t, map[string]string{
		"10.0.0.1:1080\x1falice": good,
		"10.0.0.2:1080\x1fbob":   good,
		"10.0.0.2:1080\x1fcarol": other,
		"10.0.0.3:1080":          other,
		"10.0.0.3:1080\x1fdave":  good,
	})

	// a bare address resolves to its single credentialed identity
	cred, err := resolveClaimCredential(docopt.Opts{"--store-client": "10.0.0.1:1080"})
	if err != nil || cred.ByJwt != good {
		t.Fatalf("unique address match: cred=%+v err=%v", cred, err)
	}
	// the full key still works
	cred, err = resolveClaimCredential(docopt.Opts{"--store-client": "10.0.0.2:1080\x1fcarol"})
	if err != nil || cred.ByJwt != other {
		t.Fatalf("full key: cred=%+v err=%v", cred, err)
	}
	// an address shared by two accounts is ambiguous and must not guess
	_, err = resolveClaimCredential(docopt.Opts{"--store-client": "10.0.0.2:1080"})
	if err == nil || !strings.Contains(err.Error(), "2 identities") {
		t.Fatalf("shared address must be refused as ambiguous, got %v", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "bob") || strings.Contains(err.Error(), "carol") {
		t.Fatalf("the refusal must not echo proxy usernames: %v", err)
	}
	// an exact bare key wins over credentialed siblings
	cred, err = resolveClaimCredential(docopt.Opts{"--store-client": "10.0.0.3:1080"})
	if err != nil || cred.ByJwt != other {
		t.Fatalf("exact bare key must win: cred=%+v err=%v", cred, err)
	}
}
