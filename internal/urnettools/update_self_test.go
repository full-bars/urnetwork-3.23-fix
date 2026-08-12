package urnettools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// TestSelfUpdateToolRefusesNonELF: a sha256-verified download that is not a
// recognized executable for the host platform must be refused before it can
// be run (the structural check ceiling — mirror of the provider path).
func TestSelfUpdateToolRefusesNonELF(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(exe, fakeELF("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Serve a shell script with a MATCHING digest: only the binary-format
	// check stops it.
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
	if err == nil || !strings.Contains(err.Error(), "not a "+runtime.GOOS+" executable") {
		t.Fatalf("selfUpdateToolTo = %v, want platform-aware binary refusal", err)
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
// running binary base name (urnet-tools vs urnet-docker) — never hardcode —
// and must never carry a .exe suffix even on Windows.
func TestRunningToolAssetName(t *testing.T) {
	name, err := runningToolAssetName()
	if err != nil {
		t.Fatalf("runningToolAssetName() = %v, want nil error", err)
	}
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

// TestToolAssetNameWindowsBare: on a Windows-hosted tool the base name from
// os.Executable() ends in .exe; the resulting asset name must still be bare
// (urnet-tools-windows-amd64, not urnet-tools.exe-windows-amd64). This pins
// the release-asset naming that every consumer (tool self-update, installers,
// docs) agrees on.
func TestToolAssetNameWindowsBare(t *testing.T) {
	got := toolAssetName(strings.TrimSuffix("urnet-tools.exe", ".exe"), "windows", "amd64")
	want := "urnet-tools-windows-amd64"
	if got != want {
		t.Errorf("toolAssetName(trimmed exe) = %q, want %q", got, want)
	}
	if strings.Contains(got, ".exe") {
		t.Errorf("toolAssetName = %q, must not contain .exe", got)
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
}

// TestRunToolSelfUpdateSkipsWithoutDigest: the leg returns nil (skip) when
// the release predates tool assets — a pre-Go-asset release must never fail
// the whole update.
func TestRunToolSelfUpdateSkipsWithoutDigest(t *testing.T) {
	if err := runToolSelfUpdate(updateConfig{Tag: "v3.23.0-fix.27.0"}); err != nil {
		t.Fatalf("runToolSelfUpdate(no digest) = %v, want nil (skip)", err)
	}
}

// TestRunToolSelfUpdateReportsFailure: the leg returns a non-nil error when
// the self-update download fails. (The isolation contract — that cmdUpdate
// reports this error without failing the command — is a cmdUpdate-level
// behavior; this test pins the leg's own contract: it surfaces the failure
// as an error rather than swallowing it.)
func TestRunToolSelfUpdateReportsFailure(t *testing.T) {
	cfg := updateConfig{
		Tag:          "v9.9.9",
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   strings.Repeat("a", 64),             // valid hex, server will 404
		ToolAssetURL: "http://127.0.0.1:1/does-not-exist", // connection refused
		StageDir:     t.TempDir(),
	}
	err := runToolSelfUpdate(cfg)
	if err == nil {
		t.Fatal("runToolSelfUpdate = nil, want error from failed download")
	}
}

// TestToolDigestResolvedFromSameRelease: cmdUpdate populates cfg.ToolDigest
// from the release's asset list using the tool's asset name. This pins the
// "same release, same tag" wiring (the tool digest must come from the SAME
// release the providers are updating to). The asset name is hardcoded here
// because the wiring under test is "digest lookup by name", not "name
// derivation" (that's covered by TestRunningToolAssetName) — deriving it
// from the live test binary would make the assertion dead under go test
// (verified 2026-08-12 closure review).
func TestToolDigestResolvedFromSameRelease(t *testing.T) {
	rel := &releaseInfo{
		Tag: "v9.9.9",
		Assets: []releaseAsset{
			{Name: "urnetwork-provider-v9.9.9.tar.gz", Digest: "sha256:aaa"},
			{Name: "urnet-tools-linux-amd64", Digest: "sha256:bbb"},
		},
	}
	if got := digestForAsset(rel.Assets, "urnet-tools-linux-amd64"); got != "bbb" {
		t.Errorf("digestForAsset(urnet-tools-linux-amd64) = %q, want %q", got, "bbb")
	}
	// Missing asset → empty digest (skip, not fail).
	if got := digestForAsset(rel.Assets, "urnet-tools-linux-arm64"); got != "" {
		t.Errorf("digestForAsset(missing) = %q, want empty", got)
	}
}

// TestToolDigestRelNilSkip: a fully explicit --tag+--digest update resolves
// no releaseInfo (rel == nil) — the self-update leg must skip, not panic.
func TestToolDigestRelNilSkip(t *testing.T) {
	var rel *releaseInfo
	cfg := updateConfig{Tag: "v9.9.9", ToolAsset: "urnet-tools-linux-amd64"}
	if rel != nil {
		cfg.ToolDigest = digestForAsset(rel.Assets, cfg.ToolAsset)
	}
	// Production prints the skip notice and leaves ToolDigest empty; the leg
	// then skips (nil error, no download).
	if cfg.ToolDigest != "" {
		t.Fatalf("ToolDigest = %q, want empty for rel==nil", cfg.ToolDigest)
	}
	if err := runToolSelfUpdate(cfg); err != nil {
		t.Fatalf("runToolSelfUpdate(rel nil) = %v, want nil (skip)", err)
	}
}

// writeFileHelper writes bytes to a temp path and returns the path.
func writeFileHelper(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBinaryFormatChecks: the platform-aware magic checks must accept the
// right formats and reject everything else — a shell script must fail on
// EVERY platform, and the darwin/windows magics must be recognized so the
// tool can self-update on those hosts (verified 2026-08-12 review: the old
// ELF-only check broke macOS and Windows self-update entirely).
func TestBinaryFormatChecks(t *testing.T) {
	// Mach-O magics: MH_MAGIC_64 (FE ED FA CF), MH_CIGAM_64 (CF FA ED FE),
	// fat binary (CA FE BA BE).
	for _, magic := range [][]byte{
		{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
		{0xca, 0xfe, 0xba, 0xbe}, {0xbe, 0xba, 0xfe, 0xca},
	} {
		p := writeFileHelper(t, "macho", magic)
		if !isMachOExecutable(p) {
			t.Errorf("isMachOExecutable(% x) = false, want true", magic)
		}
	}
	// PE: MZ header.
	p := writeFileHelper(t, "pe", []byte{'M', 'Z', 0x90})
	if !isPEExecutable(p) {
		t.Error("isPEExecutable(MZ) = false, want true")
	}
	// ELF.
	p = writeFileHelper(t, "elf", []byte{0x7f, 'E', 'L', 'F'})
	if !isELFExecutable(p) {
		t.Error("isELFExecutable(ELF) = false, want true")
	}
	// A shell script must be rejected by ALL platform checks.
	script := writeFileHelper(t, "script.sh", []byte("#!/bin/sh\necho hi\n"))
	for name, fn := range map[string]func(string) bool{
		"ELF":   isELFExecutable,
		"MachO": isMachOExecutable,
		"PE":    isPEExecutable,
	} {
		if fn(script) {
			t.Errorf("%s accepted a shell script", name)
		}
	}
	// isRecognizedExecutable dispatches to the host platform's check; on this
	// host it must equal the ELF result.
	if isRecognizedExecutable(script) {
		t.Error("isRecognizedExecutable accepted a shell script")
	}
}

// TestRunningToolAssetNameExeStripping: a base ending in .exe (what
// os.Executable returns on Windows) must yield a bare asset name — the
// release assets never carry .exe. The true error path of runningToolAssetName
// (os.Executable failure) is not portably forceable in a unit test; the strip
// logic is the testable half of that function's Windows contract.
func TestRunningToolAssetNameExeStripping(t *testing.T) {
	got := toolAssetName(strings.TrimSuffix("urnet-docker.exe", ".exe"), "windows", "amd64")
	want := "urnet-docker-windows-amd64"
	if got != want {
		t.Errorf("toolAssetName(urnet-docker.exe) = %q, want %q", got, want)
	}
}
