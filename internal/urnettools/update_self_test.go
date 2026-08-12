package urnettools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestToolAssetName: the release asset name for a tool binary follows the
// provider pattern (<base>-<os>-<arch>) and never carries a .exe suffix —
// release assets are bare binaries, the uploader names them.
func TestToolAssetName(t *testing.T) {
	cases := []struct {
		base, goos, arch string
		want             string
	}{
		{"urnet-tools", "linux", "amd64", "urnet-tools-linux-amd64"},
		{"urnet-tools", "linux", "arm64", "urnet-tools-linux-arm64"},
		{"urnet-docker", "linux", "amd64", "urnet-docker-linux-amd64"},
		{"urnet-docker", "linux", "arm64", "urnet-docker-linux-arm64"},
		{"urnet-tools", "darwin", "arm64", "urnet-tools-darwin-arm64"},
		{"urnet-tools", "windows", "amd64", "urnet-tools-windows-amd64"},
	}
	for _, c := range cases {
		got := toolAssetName(c.base, c.goos, c.arch)
		if got != c.want {
			t.Errorf("toolAssetName(%q,%q,%q) = %q, want %q", c.base, c.goos, c.arch, got, c.want)
		}
	}
}

// fakeELF returns bytes that pass the isELFExecutable structural check
// without being a real binary (magic + padding).
func fakeELF(payload string) []byte {
	b := append([]byte{0x7f, 'E', 'L', 'F'}, []byte(payload)...)
	return b
}

// serveTool spins up an httptest server serving toolBytes as the release
// asset, returning the server, the URL, and the sha256 hex of the bytes.
func serveTool(t *testing.T, toolBytes []byte) (*httptest.Server, string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(toolBytes)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(toolBytes)
	return srv, srv.URL, hex.EncodeToString(sum[:])
}

// TestSelfUpdateToolRefusesMissingDigest: a release that has no tool asset
// (or no digest for it) must refuse, not silently skip — the binary would be
// downloaded from the attacker-visible URL and executed.
func TestSelfUpdateToolRefusesMissingDigest(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   "", // release predates tool assets
		ToolAssetURL: "https://example.invalid/urnet-tools",
		StageDir:     t.TempDir(),
	}
	err := selfUpdateToolTo(exe, cfg)
	if err == nil || !strings.Contains(err.Error(), "no sha256 digest") {
		t.Fatalf("selfUpdateToolTo = %v, want refusal (missing digest)", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeELF("current")) {
		t.Fatal("original binary was modified on a refused update")
	}
}

// TestSelfUpdateToolRefusesBadDigest: a digest mismatch must abort with the
// file left untouched.
func TestSelfUpdateToolRefusesBadDigest(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, url, _ := serveTool(t, fakeELF("new"))
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   strings.Repeat("0", 64), // wrong
		ToolAssetURL: url,
		StageDir:     t.TempDir(),
	}
	err := selfUpdateToolTo(exe, cfg)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("selfUpdateToolTo = %v, want sha256 mismatch", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeELF("current")) {
		t.Fatal("original binary was modified on a failed verification")
	}
}

// TestSelfUpdateToolRefusesNonELF: a sha256-verified download that is not an
// ELF executable must be refused before it can be run (the structural check
// ceiling — mirror of the provider path).
func TestSelfUpdateToolRefusesNonELF(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Serve a shell script with a MATCHING digest: only the ELF check stops it.
	script := []byte("#!/bin/sh\necho pwned\n")
	_, url, digest := serveTool(t, script)
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   digest,
		ToolAssetURL: url,
		StageDir:     t.TempDir(),
	}
	err := selfUpdateToolTo(exe, cfg)
	if err == nil || !strings.Contains(err.Error(), "not an ELF executable") {
		t.Fatalf("selfUpdateToolTo = %v, want non-ELF refusal", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeELF("current")) {
		t.Fatal("original binary was modified on a refused update")
	}
}

// TestSelfUpdateToolSwapsBinary: happy path — digest verified, ELF check
// passes, the old binary is backed up and the new bytes land at the same
// path (rename, not in-place truncate).
func TestSelfUpdateToolSwapsBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := fakeELF("new-version")
	_, url, digest := serveTool(t, newBytes)
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   digest,
		ToolAssetURL: url,
		StageDir:     t.TempDir(),
	}
	if err := selfUpdateToolTo(exe, cfg); err != nil {
		t.Fatalf("selfUpdateToolTo = %v, want nil", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, newBytes) {
		t.Fatal("binary was not replaced with the verified asset")
	}
	// A timestamped backup of the old binary must exist next to it.
	matches, err := filepath.Glob(exe + ".bak-*")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no backup created (%v, %v)", matches, err)
	}
	backup, _ := os.ReadFile(matches[0])
	if !bytes.Equal(backup, fakeELF("current")) {
		t.Fatal("backup does not contain the previous binary")
	}
}

// TestSelfUpdateToolSkipsWhenAlreadyCurrent: if the installed binary already
// matches the release digest, the download is skipped entirely — the tool
// must be idempotent across repeated update calls.
func TestSelfUpdateToolSkipsWhenAlreadyCurrent(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	cur := fakeELF("already-new")
	if err := os.WriteFile(exe, cur, 0o755); err != nil {
		t.Fatal(err)
	}
	// Digest equals the CURRENT file; the server should never be hit.
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(cur)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(cur)
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   hex.EncodeToString(sum[:]),
		ToolAssetURL: srv.URL,
		StageDir:     t.TempDir(),
	}
	if err := selfUpdateToolTo(exe, cfg); err != nil {
		t.Fatalf("selfUpdateToolTo = %v, want nil", err)
	}
	if hits != 0 {
		t.Fatalf("server hit %d times, want 0 (already current)", hits)
	}
}

// TestToolSelfUpdateURLShape pins the release download URL the tool uses for
// its own asset — installers and docs must match it exactly.
func TestToolSelfUpdateURLShape(t *testing.T) {
	got := toolAssetURL("v3.23.0-fix.28.0", "urnet-tools-linux-amd64")
	want := "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v3.23.0-fix.28.0/urnet-tools-linux-amd64"
	if got != want {
		t.Errorf("toolAssetURL = %q, want %q", got, want)
	}
}

// TestRunningToolAssetName: the wrapper must derive the asset from the actual
// running binary base name (urnet-tools vs urnet-docker) — never hardcode.
func TestRunningToolAssetName(t *testing.T) {
	name := runningToolAssetName()
	// Shape: <running-binary-base>-<goos>-<goarch>. The base comes from
	// os.Executable() (in tests that's the test binary, so only the suffix
	// is stable); the GOOS/GOARCH suffix must match the host.
	wantSuffix := "-" + runtime.GOOS + "-" + runtimeGOARCH()
	if !strings.HasSuffix(name, wantSuffix) {
		t.Errorf("runningToolAssetName() = %q, want suffix %q", name, wantSuffix)
	}
	if strings.Contains(name, ".exe") {
		t.Errorf("runningToolAssetName() = %q, must not contain .exe (release assets are bare)", name)
	}
}

// TestSelfUpdateToolStageDirRequired: staging must be on a caller-provided
// real-disk dir; an empty StageDir must fail loudly instead of defaulting to
// /tmp (the tmpfs overflow class).
func TestSelfUpdateToolStageDirRequired(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := fakeELF("new-version")
	_, url, digest := serveTool(t, newBytes)
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   digest,
		ToolAssetURL: url,
		StageDir:     "", // must be rejected
	}
	err := selfUpdateToolTo(exe, cfg)
	if err == nil {
		t.Fatal("empty StageDir accepted, want error")
	}
	if !strings.Contains(err.Error(), "stage") {
		t.Fatalf("error = %v, want stage-dir error", err)
	}
	_ = fmt.Sprint() // keep fmt imported for future assertions
}
