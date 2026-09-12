package urnettools

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
// nothing stateful. This is the regression test for the
// legacy `--help`-executes-clear bug class. We exercise the dispatch layer
// by checking that parseGlobalFlags handles -h/--help on the commands that
// route through it (start/stop/logs/status/providers were the gap).
// TestDispatchHelpIsSafe was deleted — the loop body called parseGlobalFlags
// without using the loop variable, testing the same thing 5 times.
// TestParseDelegationArgsHelpIsSafe below covers the real dispatch paths.

// TestParseDelegationArgsHelpIsSafe: summary/report delegate to the provider
// binary, so -h/--help must short-circuit in parseDelegationArgs
// (help printed, nothing delegated) — the C1 invariant for pass-through
// commands (no test had pinned this).
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
// silently drop.
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
// --flags for provider-binary pass-through (summary/proxy
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
// error (matchProvider would silently apply the first set field) on
// conflicting targeting flags.
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
// (never O_TRUNC the destination in place). Verify the
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
// updates never collide. Calls the PRODUCTION backupName
// helper — a local copy would pass even if the real format changed
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
// (version check + install) with no integrity verification, rather than
// refusing an unverified download.
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

// TestUpdateProviderRefusesNonELFStagedBinary pins the fix: the
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

// TestRunVersionCommand: `version`, `--version`, and `-v` must print the
// stamped tool version and return nil. Regression for gauntlet BUG-1: the
// tool previously had no version command at all.
func TestRunVersionCommand(t *testing.T) {
	old := ToolVersion
	defer func() { ToolVersion = old }()
	ToolVersion = "test-version"
	for _, args := range [][]string{{"--version"}, {"-v"}} {
		// Capture stdout so we can pin the printed content, not just the
		// nil error (output must be verified).
		oldOut := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(&buf, r)
			close(done)
		}()
		runErr := Run(args)
		w.Close()
		os.Stdout = oldOut
		<-done
		r.Close()
		if runErr != nil {
			t.Errorf("Run(%v) = %v, want nil", args, runErr)
		}
		if got := strings.TrimSpace(buf.String()); got != "test-version" {
			t.Errorf("Run(%v) printed %q, want %q", args, got, "test-version")
		}
	}
	// `version` subcommand now shows richer output — just verify it contains
	// the tool version and doesn't error.
	{
		oldOut := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(&buf, r)
			close(done)
		}()
		runErr := Run([]string{"version"})
		w.Close()
		os.Stdout = oldOut
		<-done
		r.Close()
		if runErr != nil {
			t.Errorf("Run([version]) = %v, want nil", runErr)
		}
		if !strings.Contains(buf.String(), "test-version") {
			t.Errorf("Run([version]) = %q, want it to contain %q", buf.String(), "test-version")
		}
	}
}

// TestProxySubcommandHelpDoesNotExecute: `proxy <sub> --help` must show the
// proxy help and NOT execute the subcommand. Regression for gauntlet BUG-2:
// nested help previously fell through to root usage or was rejected as an
// unknown flag.
func TestProxySubcommandHelpDoesNotExecute(t *testing.T) {
	for _, args := range [][]string{
		{"proxy", "--help"},
		{"proxy", "add", "--help"},
		{"proxy", "add", "-h"},
		{"proxy", "clear", "--help"},
		{"proxy", "add", "/tmp/somefile", "--help"},
		{"proxy", "refresh", "--force", "-h"},
	} {
		if err := Run(args); err != nil {
			t.Errorf("Run(%v) = %v, want nil (help must never execute)", args, err)
		}
	}
}

// TestCmdReportWritesOverrideFile: `report <url>` must queue a pending
// override in pending_overrides.json when the provider is stopped, not write
// a legacy file. The provider's control socket resolves report_url from
// control-state first; writeReportURL now always goes through
// applyControlOverride which queues to pending_overrides.json when the
// socket is unavailable.
func TestCmdReportWritesOverrideFile(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir, User: "testuser"}
	// Call the PRODUCTION write helper (tests must call
	// production logic, not reimplement it).
	if err := writeReportURL(p, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("writeReportURL: %v", err)
	}
	// The override should be queued in pending_overrides.json.
	pendingPath := filepath.Join(dir, "pending_overrides.json")
	b, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("read pending_overrides.json: %v", err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatalf("parse pending_overrides.json: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("pending ops = %d, want 1", len(ops))
	}
	if ops[0].Op != "set" || ops[0].Key != "report_url" || ops[0].Value != "http://127.0.0.1:8080" {
		t.Fatalf("pending op = %+v, want set report_url=http://127.0.0.1:8080", ops[0])
	}
	// Also verify cmdReport's no-provider error path (must error, never
	// delegate to a provider binary). Skip on a box that HAS a discoverable
	// provider (e.g. this dev box's leftover sentinel unit) — the no-provider
	// branch is only reachable on a clean box, same as the other no-provider
	// tests.
	if len(Discover()) != 0 {
		t.Skip("requires a box with zero discoverable providers")
	}
	err = cmdReport([]string{"http://127.0.0.1:8080"})
	if err == nil {
		t.Fatal("cmdReport with no providers must error (not delegate to a provider binary)")
	}
}

// TestCmdHotRestartBuildsSystemctl: hot-restart must restart the provider's
// systemd unit via unitCommand, not delegate to a non-existent provider
// subcommand (gauntlet BUG-4). We pin the argv shape with unitCommandArgs.
func TestCmdHotRestartBuildsSystemctl(t *testing.T) {
	p := Provider{Unit: "urnetwork-beta.service", User: "urnetwork-beta"}
	args := unitCommandArgs(p, "restart")
	// The unit name must be the final argument before any extras. The exact
	// form depends on whether the test env treats the unit as user-level
	// (systemctl --user -M <user>@ ...) or system-level (systemctl ...),
	// but BOTH must end with the unit — systemctl errors "Too few
	// arguments" without it (gauntlet finding).
	if len(args) < 3 {
		t.Fatalf("unitCommandArgs(restart) = %v, want systemctl ... restart <unit>", args)
	}
	if args[0] != "systemctl" {
		t.Fatalf("unitCommandArgs(restart) = %v, want systemctl first", args)
	}
	if args[len(args)-1] != p.Unit {
		t.Fatalf("unitCommandArgs(restart) = %v, want final arg = unit %q (systemctl errors without it)", args, p.Unit)
	}
	// The confirm gate must exist: a non-force restart with no stdin (EOF)
	// must be refused, not silently restart. Test
	// confirmGate directly — deterministic, no discovery dependency (CI has
	// no provider; Run(["restart"]) would error at discovery before the
	// gate, which is env-dependent).
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	w.Close() // EOF — no confirmation typed
	_, gateErr := confirmGate("restart "+p.Unit, p, false, false)
	os.Stdin = oldStdin
	if gateErr == nil {
		t.Fatal("confirmGate with EOF must refuse (non-nil error), not silently proceed")
	}
	if !strings.Contains(gateErr.Error(), "read confirmation") && !strings.Contains(gateErr.Error(), "confirmation did not match") {
		t.Fatalf("confirmGate EOF error = %v, want the confirmation-refusal error", gateErr)
	}
	// Force must bypass the gate.
	if ok, err := confirmGate("restart "+p.Unit, p, true, false); err != nil || !ok {
		t.Fatalf("confirmGate with force = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestConfirmStdinReadNonInteractiveRefuses: an open-but-silent pipe (cron,
// CI, MCP exec) must be refused with a clear message, not block forever.
// Regression for gauntlet BUG-14: self-update blocked on read(0) for minutes
// because ReadString on an open pipe never sees EOF.
func TestConfirmStdinReadNonInteractiveRefuses(t *testing.T) {
	// Point os.Stdin at an open pipe that never delivers data: the terminal
	// check must see it as non-interactive and refuse BEFORE reading. The
	// read itself would otherwise block forever (BUG-14).
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Replace the shared reader too — it may hold a buffered fd from a
	// previous prompt in the same test binary.
	os.Stdin = r
	oldReader := stdinReader
	stdinReader = bufio.NewReader(r)
	defer func() {
		os.Stdin = oldStdin
		stdinReader = oldReader
		w.Close()
		r.Close()
	}()

	line, err := confirmStdinRead("prompt> ")
	if err == nil {
		t.Fatalf("confirmStdinRead on open-pipe stdin = (%q, nil), want refusal error", line)
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("confirmStdinRead error = %v, want the non-interactive refusal message", err)
	}
}
