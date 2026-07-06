package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMintAndValidateOnboardToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	token, expiry, err := mintOnboardToken(dir, now, onboardTokenTTL)
	if err != nil {
		t.Fatalf("mintOnboardToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}
	if !expiry.After(now) {
		t.Fatalf("expiry = %v, want it after mint time %v", expiry, now)
	}

	if !validateOnboardToken(dir, token, now) {
		t.Error("expected a freshly minted token to validate")
	}
}

func TestValidateOnboardToken_ExpiresAfterTTL(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	token, expiry, err := mintOnboardToken(dir, now, 15*time.Minute)
	if err != nil {
		t.Fatalf("mintOnboardToken: %v", err)
	}

	if !validateOnboardToken(dir, token, expiry.Add(-time.Second)) {
		t.Error("expected token to still be valid 1s before expiry")
	}
	if validateOnboardToken(dir, token, expiry.Add(time.Second)) {
		t.Error("expected token to be invalid 1s after expiry")
	}
}

func TestValidateOnboardToken_UnknownTokenFails(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if _, _, err := mintOnboardToken(dir, now, onboardTokenTTL); err != nil {
		t.Fatalf("mintOnboardToken: %v", err)
	}

	if validateOnboardToken(dir, "not-a-real-token", now) {
		t.Error("expected an unminted token to fail validation")
	}
}

func TestValidateOnboardToken_NoTokenFileFails(t *testing.T) {
	dir := t.TempDir()
	if validateOnboardToken(dir, "anything", time.Now()) {
		t.Error("expected validation to fail when onboard.tokens doesn't exist yet")
	}
}

// TestMintOnboardToken_MultipleTokensCoexist is the deliberate design
// decision: onboard-cmd run twice (e.g. a second terminal, or re-running
// mid fleet-rollout) must not invalidate the first token. Both stay valid
// independently within their own 15-minute windows.
func TestMintOnboardToken_MultipleTokensCoexist(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	token1, _, err := mintOnboardToken(dir, now, onboardTokenTTL)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	token2, _, err := mintOnboardToken(dir, now.Add(time.Minute), onboardTokenTTL)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}

	checkAt := now.Add(2 * time.Minute)
	if !validateOnboardToken(dir, token1, checkAt) {
		t.Error("expected the first token to still be valid")
	}
	if !validateOnboardToken(dir, token2, checkAt) {
		t.Error("expected the second token to still be valid")
	}
}

// TestMintOnboardToken_PrunesExpiredLines confirms the token file doesn't
// grow unbounded: minting again after an earlier token has expired should
// drop that earlier line.
func TestMintOnboardToken_PrunesExpiredLines(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	oldToken, _, err := mintOnboardToken(dir, now, time.Minute)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}

	later := now.Add(2 * time.Minute) // past the first token's 1-minute TTL
	newToken, _, err := mintOnboardToken(dir, later, onboardTokenTTL)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}

	if validateOnboardToken(dir, oldToken, later) {
		t.Error("expected the expired token to be pruned and invalid")
	}
	if !validateOnboardToken(dir, newToken, later) {
		t.Error("expected the newly minted token to be valid")
	}
}

func TestHandleCACert_ValidToken(t *testing.T) {
	dir := t.TempDir()
	ca, err := deriveCA("test-password", randomBytes(t, 32))
	if err != nil {
		t.Fatalf("deriveCA: %v", err)
	}
	token, _, err := mintOnboardToken(dir, time.Now(), onboardTokenTTL)
	if err != nil {
		t.Fatalf("mintOnboardToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/ca-cert?token="+token, nil)
	w := httptest.NewRecorder()
	handleCACert(dir, ca)(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
		t.Errorf("body = %q, want it to contain the CA PEM", w.Body.String())
	}
	caFP, _ := ca.caFingerprint()
	if !strings.Contains(w.Body.String(), caFP) {
		t.Errorf("body = %q, want it to contain the CA fingerprint %q", w.Body.String(), caFP)
	}
}

func TestHandleCACert_InvalidTokenRejected(t *testing.T) {
	dir := t.TempDir()
	ca, err := deriveCA("test-password", randomBytes(t, 32))
	if err != nil {
		t.Fatalf("deriveCA: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/ca-cert?token=bogus", nil)
	w := httptest.NewRecorder()
	handleCACert(dir, ca)(w, req)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403 for an invalid token", w.Code)
	}
}

func TestHandleCACert_MissingTokenRejected(t *testing.T) {
	dir := t.TempDir()
	ca, err := deriveCA("test-password", randomBytes(t, 32))
	if err != nil {
		t.Fatalf("deriveCA: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/ca-cert", nil)
	w := httptest.NewRecorder()
	handleCACert(dir, ca)(w, req)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403 when no token is supplied", w.Code)
	}
}

func TestHandleOnboardScript_SubstitutesHostAndPort(t *testing.T) {
	handler := handleOnboardScript("8443")

	req := httptest.NewRequest("GET", "/onboard.sh", nil)
	req.Host = "hub.example.com:8080" // request arrives on the plain-HTTP port
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "https://hub.example.com:8443") {
		t.Errorf("body does not contain the expected HTTPS URL with the TLS port substituted; body: %s", body)
	}
	if strings.Contains(body, "https://hub.example.com:8080") {
		t.Error("body incorrectly carries the request's plain-HTTP port instead of the TLS port")
	}
}

// TestHandleOnboardScript_Port443OmitsExplicitPort locks in the fix for the
// bug where tlsPort=="443" fell back to the raw request Host (which still
// carried the plain-HTTP port) instead of correctly omitting the port.
func TestHandleOnboardScript_Port443OmitsExplicitPort(t *testing.T) {
	handler := handleOnboardScript("443")

	req := httptest.NewRequest("GET", "/onboard.sh", nil)
	req.Host = "hub.example.com:8080"
	w := httptest.NewRecorder()
	handler(w, req)

	body := w.Body.String()
	// Note: the script's separate http-fallback fetch line has its own
	// hardcoded ":8080" unrelated to this fix — check the HUB_URL
	// assignment specifically, not the whole script body.
	if !strings.Contains(body, `HUB_URL="https://hub.example.com"`) {
		t.Errorf("body = %q, want HUB_URL set to https://hub.example.com with no port", body)
	}
	if strings.Contains(body, `HUB_URL="https://hub.example.com:8080"`) {
		t.Error("HUB_URL incorrectly carries the request's plain-HTTP port when tlsPort is 443")
	}
}
