package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"testing"
	"time"
)

func TestDeriveCA_DeterministicAcrossCalls(t *testing.T) {
	password, salt := "test-password", randomBytes(t, 32)

	ca1, err := deriveCA(password, salt)
	if err != nil {
		t.Fatalf("first deriveCA: %v", err)
	}
	ca2, err := deriveCA(password, salt)
	if err != nil {
		t.Fatalf("second deriveCA: %v", err)
	}

	if string(ca1.certPEM) != string(ca2.certPEM) {
		t.Error("certPEM differs between calls")
	}
	if ca1.caFingerprint() != ca2.caFingerprint() {
		t.Error("fingerprint differs between calls")
	}
	// Verify same public key
	if !ca1.key.Public().(ed25519.PublicKey).Equal(ca2.key.Public().(ed25519.PublicKey)) {
		t.Error("public keys differ")
	}
}

func TestDeriveCA_DifferentPasswordDifferentCA(t *testing.T) {
	salt := randomBytes(t, 32)
	ca1, _ := deriveCA("password1", salt)
	ca2, _ := deriveCA("password2", salt)

	if ca1.caFingerprint() == ca2.caFingerprint() {
		t.Error("same fingerprint for different passwords")
	}
}

func TestDeriveCA_DifferentSaltDifferentCA(t *testing.T) {
	password := "test-password"
	ca1, _ := deriveCA(password, randomBytes(t, 32))
	ca2, _ := deriveCA(password, randomBytes(t, 32))

	if ca1.caFingerprint() == ca2.caFingerprint() {
		t.Error("same fingerprint for different salts")
	}
}

func TestIssueLeaf_VerifiesAgainstCA(t *testing.T) {
	ca, err := deriveCA("test-password", randomBytes(t, 32))
	if err != nil {
		t.Fatalf("deriveCA: %v", err)
	}

	leaf, err := ca.issueLeaf([]string{"test.local"}, 48*time.Hour)
	if err != nil {
		t.Fatalf("issueLeaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)

	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	opts := x509.VerifyOptions{
		Roots:         pool,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Intermediates: x509.NewCertPool(),
	}
	_, err = leafCert.Verify(opts)
	if err != nil {
		t.Errorf("leaf does not verify against CA: %v", err)
	}
}

func TestIssueLeaf_WrongCAFails(t *testing.T) {
	ca1, _ := deriveCA("password1", randomBytes(t, 32))
	ca2, _ := deriveCA("password2", randomBytes(t, 32))

	leaf, _ := ca1.issueLeaf([]string{"test.local"}, 48*time.Hour)
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca2.certPEM)

	opts := x509.VerifyOptions{
		Roots:         pool,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := leafCert.Verify(opts); err == nil {
		t.Error("wrong CA should not verify")
	}
}

func TestLoadOrCreateCAMaterial_GeneratesOnceThenStable(t *testing.T) {
	tmp := t.TempDir()

	pw1, salt1, gen1, err := loadOrCreateCAMaterial(tmp)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !gen1 {
		t.Error("expected generatedPassword=true on first call")
	}

	// Second call should read existing
	pw2, salt2, gen2, err := loadOrCreateCAMaterial(tmp)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if gen2 {
		t.Error("expected generatedPassword=false on second call")
	}
	if pw1 != pw2 || string(salt1) != string(salt2) {
		t.Error("password or salt changed between calls")
	}
}

func TestLeafSANs_ContainsHostname(t *testing.T) {
	sans := leafSANs()
	if len(sans) == 0 {
		t.Error("expected at least some SANs")
	}
	// Should contain localhost and 127.0.0.1
	found := make(map[string]bool)
	for _, s := range sans {
		found[s] = true
	}
	if !found["localhost"] {
		t.Error("missing localhost in SANs")
	}
	if !found["127.0.0.1"] {
		t.Error("missing 127.0.0.1 in SANs")
	}
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}