package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	if fp1, _ := ca1.caFingerprint(); fp1 != func() string { fp2, _ := ca2.caFingerprint(); return fp2 }() {
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

	fp1, _ := ca1.caFingerprint()
	fp2, _ := ca2.caFingerprint()
	if fp1 == fp2 {
		t.Error("same fingerprint for different passwords")
	}
}

func TestDeriveCA_DifferentSaltDifferentCA(t *testing.T) {
	password := "test-password"
	ca1, _ := deriveCA(password, randomBytes(t, 32))
	ca2, _ := deriveCA(password, randomBytes(t, 32))

	fp1, _ := ca1.caFingerprint()
	fp2, _ := ca2.caFingerprint()
	if fp1 == fp2 {
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
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := leafCert.Verify(opts); err == nil {
		t.Error("wrong CA should not verify")
	}
}

// TestIssueLeaf_ClassifiesIPSANsCorrectly guards against IP-literal SANs
// (e.g. "127.0.0.1", interface IPs from leafSANs) ending up in DNSNames
// instead of IPAddresses — the wrong general-name type per x509, which
// breaks hostname verification by IP for any consumer that checks it (a
// browser, openssl s_client, or a future `hub test` command), even though
// verifyHubChain itself skips hostname checking.
func TestIssueLeaf_ClassifiesIPSANsCorrectly(t *testing.T) {
	ca, err := deriveCA("test-password", randomBytes(t, 32))
	if err != nil {
		t.Fatalf("deriveCA: %v", err)
	}

	leaf, err := ca.issueLeaf([]string{"hub.example", "127.0.0.1", "10.0.0.5"}, 48*time.Hour)
	if err != nil {
		t.Fatalf("issueLeaf: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if len(leafCert.IPAddresses) != 2 {
		t.Errorf("IPAddresses = %v, want 2 entries (127.0.0.1, 10.0.0.5)", leafCert.IPAddresses)
	}
	for _, dns := range leafCert.DNSNames {
		if net.ParseIP(dns) != nil {
			t.Errorf("DNSNames = %v, contains an IP literal %q that belongs in IPAddresses", leafCert.DNSNames, dns)
		}
	}
	if len(leafCert.DNSNames) != 1 || leafCert.DNSNames[0] != "hub.example" {
		t.Errorf("DNSNames = %v, want exactly [\"hub.example\"]", leafCert.DNSNames)
	}

	if err := leafCert.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("VerifyHostname(127.0.0.1) failed: %v — IP SAN not recognized", err)
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

func TestLoadOrCreateCAMaterial_PartialMaterialFails(t *testing.T) {
	dir := t.TempDir()

	// Fresh start: should succeed and generate both files
	pw, _, _, err := loadOrCreateCAMaterial(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) == 0 {
		t.Fatal("expected non-empty password")
	}

	// Delete salt only — must fail, not regenerate
	os.Remove(filepath.Join(dir, "hub.salt"))
	_, _, _, err = loadOrCreateCAMaterial(dir)
	if err == nil {
		t.Fatal("expected error when salt is missing but password exists")
	}

	// Delete password too — fresh start again, must succeed
	os.Remove(filepath.Join(dir, "hub.password"))
	_, _, _, err = loadOrCreateCAMaterial(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Now delete password only — must fail
	os.Remove(filepath.Join(dir, "hub.password"))
	_, _, _, err = loadOrCreateCAMaterial(dir)
	if err == nil {
		t.Fatal("expected error when password is missing but salt exists")
	}
}

func TestLoadOrCreateCAMaterial_RejectsShortPassword(t *testing.T) {
	dir := t.TempDir()
	// Write a too-short password alongside a valid salt
	_ = os.WriteFile(filepath.Join(dir, "hub.password"), []byte("abc"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "hub.salt"), []byte("deadbeef"), 0600)

	_, _, _, err := loadOrCreateCAMaterial(dir)
	if err == nil {
		t.Fatal("expected error for password shorter than 8 chars")
	}
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCaFingerprint_ErrorOnCorruptPEM(t *testing.T) {
	ca := &hubCA{certPEM: []byte("this is not a PEM certificate")}
	_, err := ca.caFingerprint()
	if err == nil {
		t.Fatal("expected error on corrupt PEM")
	}
}

func TestLeafSANs_IncludesIPv6Loopback(t *testing.T) {
	// The loopback check in leafSANs is independent of host interfaces.
	sans := leafSANs()
	found := false
	for _, s := range sans {
		if s == "::1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected ::1 in leaf SANs")
	}
}

func TestLeafSANs_CappedAtMax(t *testing.T) {
	sans := leafSANs()
	if len(sans) > leafSANMax {
		t.Errorf("leafSANs returned %d entries, expected at most %d", len(sans), leafSANMax)
	}
}

func TestSweepStaleTmpFiles_RemovesTmpFiles(t *testing.T) {
	dir := t.TempDir()
	// Create a few .tmp files
	for _, name := range []string{"hub.password.tmp", "hub.salt.tmp", "ca.crt.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Create a non-empty, non-.tmp file that should survive
	realFile := filepath.Join(dir, "hub.password")
	if err := os.WriteFile(realFile, []byte("real"), 0600); err != nil {
		t.Fatal(err)
	}

	sweepStaleTmpFiles(dir)

	for _, name := range []string{"hub.password.tmp", "hub.salt.tmp", "ca.crt.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("expected %s to be removed", name)
		}
	}
	if _, err := os.Stat(realFile); err != nil {
		t.Errorf("expected hub.password to survive sweep: %v", err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}
