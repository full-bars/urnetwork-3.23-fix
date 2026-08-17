package urnettools

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDecodeJWT verifies network identity extraction from a real JWT shape.
func TestDecodeJWT(t *testing.T) {
	// Build a real JWT: base64url(payload) with the tacogonzalez3000 shape.
	payload := `{"network_name":"tacogonzalez3000","network_id":"019c3c0c-436c-6b8b-68a3-2d21dd48a50c","exp":1788344762}`
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	jwt := "header." + payloadB64 + ".sig"
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte(jwt), 0o644); err != nil {
		t.Fatal(err)
	}
	net, id, exp, err := decodeJWT(path)
	if err != nil {
		t.Fatalf("decodeJWT: %v", err)
	}
	if net != "tacogonzalez3000" {
		t.Errorf("network_name = %q, want tacogonzalez3000", net)
	}
	if id != "019c3c0c-436c-6b8b-68a3-2d21dd48a50c" {
		t.Errorf("network_id = %q", id)
	}
	want := time.Unix(1788344762, 0)
	if !exp.Equal(want) {
		t.Errorf("exp = %v, want %v", exp, want)
	}
}

// TestDecodeJWTNotAJWT covers malformed input: must return error, not panic.
func TestDecodeJWTNotAJWT(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte("not-a-jwt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := decodeJWT(path); err == nil {
		t.Fatal("expected error for non-JWT input")
	}
}

// TestDecodeJWTMissingFile: missing jwt should surface the read error.
func TestDecodeJWTMissingFile(t *testing.T) {
	if _, _, _, err := decodeJWT(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestSelectTargetSingleProvider: with one provider and no target, the safe
// default is to select it.
func TestSelectTargetSingleProvider(t *testing.T) {
	providers := []Provider{{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000"}}
	p, err := selectTarget(providers, Target{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Network != "tacogonzalez3000" {
		t.Errorf("selected %s", p.Network)
	}
}

// TestSelectTargetAmbiguousRefuses: multiple providers and no target MUST
// refuse — this is the incident-class guard.
func TestSelectTargetAmbiguousRefuses(t *testing.T) {
	providers := []Provider{
		{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000"},
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Network: "beta-test"},
	}
	_, err := selectTarget(providers, Target{})
	if err == nil {
		t.Fatal("expected refusal with multiple providers and no target")
	}
	if got := err.Error(); !contains(got, "specify a target") {
		t.Errorf("error should ask for a target, got: %s", got)
	}
}

// TestSelectTargetByNetwork: explicit network targeting resolves exactly.
func TestSelectTargetByNetwork(t *testing.T) {
	providers := []Provider{
		{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000"},
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Network: "beta-test"},
	}
	p, err := selectTarget(providers, Target{Network: "tacogonzalez3000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.User != "urnet" {
		t.Errorf("selected user %s, want urnet", p.User)
	}
}

// TestSelectTargetNoMatch: a target matching nothing is an error.
func TestSelectTargetNoMatch(t *testing.T) {
	providers := []Provider{{User: "urnet", Network: "tacogonzalez3000"}}
	if _, err := selectTarget(providers, Target{Network: "nope"}); err == nil {
		t.Fatal("expected error for non-matching target")
	}
}

// TestSelectTargetZeroProviders: nothing on the box is an error, not a
// silent no-op.
func TestSelectTargetZeroProviders(t *testing.T) {
	if _, err := selectTarget(nil, Target{}); err == nil {
		t.Fatal("expected error with no providers")
	}
}

// TestIsProviderArg covers binary-name recognition.
func TestIsProviderArg(t *testing.T) {
	cases := map[string]bool{
		"/home/urnet/.local/share/urnetwork-provider/bin/urnetwork": true,
		"/home/urnetwork-beta/provider_beta":                        true,
		"provider":                                                  true,
		"provider-custom":                                           true, // legitimate suffixed variant install
		"/usr/bin/python3":                                          false,
		"/bin/bash":                                                 false,
	}
	for arg, want := range cases {
		if got := isProviderArg(arg); got != want {
			t.Errorf("isProviderArg(%q) = %v, want %v", arg, got, want)
		}
	}
}

// TestIsProviderArgExcludesKnownSiblings: units/binaries sharing the
// provider name as a PREFIX but denoting an unrelated sibling service
// (dashboard apps, the hub, the updater) must never be treated as a
// provider. dashboard cases are a live fleet false-positive (2026-08-17):
// provider-dashboard{,-py,-rs}.service (unrelated monitoring services) were
// swept into discovery, flooding the same-user candidate list and blocking
// narrowToAccessible's auto-pick on a box with exactly one real provider.
func TestIsProviderArgExcludesKnownSiblings(t *testing.T) {
	nonProviders := []string{
		"provider-hub",
		"provider-update",
		"provider-dashboard",
		"provider-dashboard-py",
		"provider-dashboard-rs",
		"urnetwork-dashboard",
	}
	for _, arg := range nonProviders {
		if isProviderArg(arg) {
			t.Errorf("isProviderArg(%q) = true, want false — known non-provider sibling", arg)
		}
	}
}

// TestStateDirFor verifies HOME-based state resolution (no hardcoded path).
func TestStateDirFor(t *testing.T) {
	// stateDirFor joins with filepath.Join, so the expected path is
	// platform-dependent (forward slashes on Unix, backslashes on Windows).
	env := map[string]string{"HOME": "/home/urnet", "USER": "urnet"}
	want := filepath.Join("/home/urnet", ".urnetwork")
	if got := stateDirFor(env); got != want {
		t.Errorf("stateDirFor = %q, want %q", got, want)
	}
	if got := stateDirFor(map[string]string{}); got != "" {
		t.Errorf("expected empty state dir without HOME, got %q", got)
	}
}

// TestParsePID sanity.
func TestParsePID(t *testing.T) {
	if parsePID("1234") != 1234 {
		t.Error("parsePID(1234)")
	}
	if parsePID("abc") != 0 {
		t.Error("parsePID(abc) should be 0")
	}
	if parsePID("-5") != 0 {
		t.Error("parsePID(-5) should be 0")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && len(sub) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
