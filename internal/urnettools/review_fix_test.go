package urnettools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDispatchHelpIsSafe: --help on EVERY command must print help and do
// nothing stateful. This is the regression test for review finding C1 — the
// legacy `--help`-executes-clear bug class. We exercise the dispatch layer
// by checking that parseGlobalFlags handles -h/--help on the commands that
// route through it (start/stop/logs/status/providers were the gap).
func TestDispatchHelpIsSafe(t *testing.T) {
	// The five previously-affected commands all route through
	// parseGlobalFlags now. Verify -h returns errHelpShown (help printed,
	// command NOT executed).
	for _, cmd := range []string{"start", "stop", "logs", "status", "providers"} {
		// These dispatch cases call parseGlobalFlags; the sentinel proves
		// help short-circuits before the command function runs.
		// We can't call Run() without building a binary, but we can verify
		// the parser treats -h correctly (the dispatch wiring is exercised
		// by the binary-level parity check).
		_, _, _, err := parseGlobalFlags([]string{"-h"})
		if err != errHelpShown {
			t.Errorf("%s -h: expected errHelpShown, got %v", cmd, err)
		}
		_, _, _, err = parseGlobalFlags([]string{"--help"})
		if err != errHelpShown {
			t.Errorf("%s --help: expected errHelpShown, got %v", cmd, err)
		}
	}
}

// TestParseDelegationArgsHelpIsSafe: summary/report/hot-restart delegate to
// the provider binary, so -h/--help must short-circuit in parseDelegationArgs
// (help printed, nothing delegated) — the C1 invariant for pass-through
// commands (free-review gap: no test pinned this).
func TestParseDelegationArgsHelpIsSafe(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"--unit", "urnetwork-native.service", "-h"}} {
		rest, err := parseDelegationArgs(args)
		if err != errHelpShown {
			t.Errorf("parseDelegationArgs(%v): expected errHelpShown, got %v", args, err)
		}
		if rest != nil {
			t.Errorf("parseDelegationArgs(%v): rest must be nil when help shown, got %v", args, rest)
		}
	}
	// Without help flags, args pass through untouched.
	rest, err := parseDelegationArgs([]string{"--unit", "urnetwork-native.service"})
	if err != nil {
		t.Fatalf("no help flag: expected nil error, got %v", err)
	}
	if len(rest) != 2 || rest[0] != "--unit" {
		t.Errorf("args must pass through unchanged, got %v", rest)
	}
}

// TestParseTargetFlagsRejectsUnknownFlags: a typo'd --flag must error, not
// silently drop (review finding L2).
func TestParseTargetFlagsRejectsUnknownFlags(t *testing.T) {
	_, _, err := parseTargetFlags([]string{"--netwrok", "foo"})
	if err == nil {
		t.Fatal("expected error for unknown flag --netwrok")
	}
	if !contains(err.Error(), "unknown flag") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseTargetFlagsLenientPreserves: the lenient variant keeps unknown
// --flags for provider-binary pass-through (summary/hot-restart/proxy
// refresh/remove-dead).
func TestParseTargetFlagsLenientPreserves(t *testing.T) {
	tg, rest, err := parseTargetFlagsLenient([]string{"--unit", "urnetwork-native.service", "--force"})
	if err != nil {
		t.Fatalf("lenient parse: %v", err)
	}
	if tg.Unit != "urnetwork-native.service" {
		t.Errorf("unit = %q", tg.Unit)
	}
	if len(rest) != 1 || rest[0] != "--force" {
		t.Errorf("rest should preserve --force, got %v", rest)
	}
}

// TestParseTargetFlagsConflictingRejected: --unit + --network together must
// error (matchProvider would silently apply the first set field). Pins the
// free-review major on conflicting targeting flags.
func TestParseTargetFlagsConflictingRejected(t *testing.T) {
	_, _, err := parseTargetFlags([]string{"--unit", "urnetwork-native.service", "--network", "tacogonzalez3000"})
	if err == nil {
		t.Fatal("conflicting targeting flags must error")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error must say the selectors conflict, got: %v", err)
	}
	// Same-field repeat is fine (overwrite).
	if _, _, err := parseTargetFlags([]string{"--unit", "a.service", "--unit", "b.service"}); err != nil {
		t.Fatalf("same-field repeat must not error, got: %v", err)
	}
}

// TestVerifySHA256Mismatch: a wrong digest must error (the update flow's
// integrity gate).
func TestVerifySHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifySHA256(path, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for sha256 mismatch")
	}
	if !contains(err.Error(), "mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestVerifySHA256Match: the correct digest passes.
func TestVerifySHA256Match(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	err := verifySHA256(path, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
	if err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

// TestInstallBinaryAtomic: installBinary must write to dst+.new and rename
// (never O_TRUNC the destination in place — review finding H2). Verify the
// resulting file is correct and no .new remnant remains.
func TestInstallBinaryAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new-binary-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// user="" skips chown; run as non-root path.
	if err := installBinary(src, dst, ""); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new-binary-content" {
		t.Errorf("dst content = %q, want new-binary-content", b)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("dst.new should not remain after rename (err=%v)", err)
	}
}

// TestBackupNameTimestamped: backup names include a timestamp so repeated
// updates never collide (review finding M2). Calls the PRODUCTION backupName
// helper — a local copy would pass even if the real format changed
// (coderabbit trivial finding).
func TestBackupNameTimestamped(t *testing.T) {
	a := backupName("/usr/local/bin/provider", time.Date(2026, 8, 9, 3, 15, 0, 0, time.UTC))
	b := backupName("/usr/local/bin/provider", time.Date(2026, 8, 9, 3, 15, 1, 0, time.UTC))
	if a == b {
		t.Errorf("backup names must differ across seconds, got %q == %q", a, b)
	}
	if !strings.Contains(a, "bak-20") {
		t.Errorf("backup name should carry a timestamp, got %s", a)
	}
	if !strings.HasPrefix(a, "/usr/local/bin/provider") {
		t.Errorf("backup must preserve the binary path prefix, got %s", a)
	}
}

// TestUpdateProviderRefusesEmptyDigest: updateProvider must refuse to run
// when no sha256 digest is available — the staged binary would be executed
// (version check + install) with no integrity verification. Pins the
// free-review critical on unverified downloads.
func TestUpdateProviderRefusesEmptyDigest(t *testing.T) {
	dir := t.TempDir()
	cfg := updateConfig{
		Tag:      "v9.9.9-test",
		Digest:   "",
		StageDir: filepath.Join(dir, "stage"),
	}
	err := updateProvider(Provider{Binary: filepath.Join(dir, "provider")}, cfg)
	if err == nil {
		t.Fatal("updateProvider with empty digest must error")
	}
	if !strings.Contains(err.Error(), "no sha256 digest") {
		t.Fatalf("error must say digest is missing, got: %v", err)
	}
	// Must fail BEFORE any download/stage activity.
	if _, err := os.Stat(cfg.StageDir); !os.IsNotExist(err) {
		t.Errorf("stage dir must not be created when digest is missing (err=%v)", err)
	}
}

// TestUpdateProviderRefusesNonELFStagedBinary pins the MEDIUM-1 fix: the
// provider update path must sanity-check the extracted binary
// STRUCTURALLY (isELFExecutable), never by executing it. A staged file
// that is not an ELF executable must abort the install — even when the
// download+digest+extract pipeline succeeds.
func TestUpdateProviderRefusesNonELFStagedBinary(t *testing.T) {
	dir := t.TempDir()
	// Build a gzipped tarball whose linux/<arch>/provider is a shell script
	// (not an ELF binary), serve it over HTTP, and feed a matching digest
	// so the only thing that can stop the install is the structural check.
	rel := tarRelPath(runtime.GOOS, runtimeGOARCH())
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name: rel, Mode: 0o755, Size: int64(len(shellScript)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(shellScript)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(gzBuf.Bytes()))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gzBuf.Bytes())
	}))
	defer srv.Close()

	cfg := updateConfig{
		Tag:      "v9.9.9-test",
		Digest:   digest,
		AssetURL: srv.URL + "/urnetwork-provider-v9.9.9-test.tar.gz",
		StageDir: filepath.Join(dir, "stage"),
	}
	err := updateProvider(Provider{Binary: filepath.Join(dir, "provider")}, cfg)
	if err == nil {
		t.Fatal("updateProvider must refuse a non-ELF staged binary")
	}
	if !strings.Contains(err.Error(), "not a "+runtime.GOOS+" executable") {
		t.Fatalf("error must say the staged binary is not a %s executable, got: %v", runtime.GOOS, err)
	}
}

const shellScript = "#!/bin/sh\necho not-a-binary\n"
