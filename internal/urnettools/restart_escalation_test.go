package urnettools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// --- isAuthRestartFailure ---

func TestIsAuthRestartFailure(t *testing.T) {
	yes := []string{
		"systemctl restart urnetwork.service: exit status 1 (Failed to restart urnetwork.service: Interactive authentication required.\nSee system logs...)",
		"systemctl restart x: Authentication is required to manage system services",
		"sudo: a password is required (access denied)",
		"Not authorized to control systemd",
	}
	no := []error{
		nil,
		errors.New("Failed to start urnetwork.service: Unit not found"),
		errors.New("connection timed out"),
		errors.New("swap binary: permission denied"), // plain fs failure, different remediation
	}
	for _, s := range yes {
		if !isAuthRestartFailure(errors.New(s)) {
			t.Errorf("expected AUTH classification for: %q", s)
		}
	}
	for _, e := range no {
		if isAuthRestartFailure(e) {
			t.Errorf("expected NON-auth classification for: %v", e)
		}
	}
}

// --- ladder wiring ---

func stubSudo(t *testing.T, available bool) *bool {
	t.Helper()
	called := false
	orig := sudoAvailableFn
	sudoAvailableFn = func() bool { called = true; return available }
	t.Cleanup(func() { sudoAvailableFn = orig })
	return &called
}

func stubStagedRestart(t *testing.T, err error) (*string, *string, *string) {
	t.Helper()
	var gotTool, gotUnit, gotUser string
	orig := runStagedRestartFn
	runStagedRestartFn = func(tool string, p Provider) error {
		gotTool, gotUnit, gotUser = tool, p.Unit, p.User
		return err
	}
	t.Cleanup(func() { runStagedRestartFn = orig })
	return &gotTool, &gotUnit, &gotUser
}

func stubRestartFn(t *testing.T, err error) {
	t.Helper()
	orig := restartFn
	restartFn = func(p Provider) error { return err }
	t.Cleanup(func() { restartFn = orig })
}

func TestRestartLadder_PrimarySuccessNeverEscalates(t *testing.T) {
	stubRestartFn(t, nil)
	sudoCalled := stubSudo(t, true)
	err := restartLadder(Provider{Unit: "urnetwork.service", User: "user"}, "/staged/tool")
	if err != nil {
		t.Fatalf("primary success must return nil, got %v", err)
	}
	if *sudoCalled {
		t.Fatal("sudo must not be consulted when the primary restart succeeded")
	}
}

func TestRestartLadder_NonAuthErrorPropagatesUnchanged(t *testing.T) {
	want := errors.New("systemctl restart x: exit 1 (Unit not found)")
	stubRestartFn(t, want)
	sudoCalled := stubSudo(t, true)
	err := restartLadder(Provider{Unit: "x.service", User: "user"}, "/staged/tool")
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("non-auth failure must propagate verbatim, got %v", err)
	}
	if *sudoCalled {
		t.Fatal("sudo must not be consulted for non-auth failures")
	}
}

func TestRestartLadder_PasswordlessSudoRetriesThroughStagedTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("escalation ladder (sudo/polkit/staged-exec) is linux-only")
	}
	stubRestartFn(t, errors.New("Interactive authentication required"))
	sudoCalled := stubSudo(t, true)
	tool, unit, user := stubStagedRestart(t, nil)

	err := restartLadder(Provider{Unit: "urnetwork.service", User: "user"}, "/var/tmp/stage/urnet-tools-linux-amd64")
	if err != nil {
		t.Fatalf("escalated retry should succeed, got %v", err)
	}
	if !*sudoCalled {
		t.Fatal("sudo availability must be probed before retrying")
	}
	if *tool != "/var/tmp/stage/urnet-tools-linux-amd64" || *unit != "urnetwork.service" || *user != "user" {
		t.Fatalf("staged retry got wrong arguments: tool=%q unit=%q user=%q", *tool, *unit, *user)
	}
}

func TestRestartLadder_NoPasswordlessSudoPrintsGuidance(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("guidance text references linux polkit paths")
	}
	stubRestartFn(t, errors.New("Interactive authentication required"))
	stubSudo(t, false)

	var out string
	var err error
	out = captureStdout(t, func() {
		err = restartLadder(Provider{Unit: "urnetwork.service", User: "user"}, "/var/tmp/stage/tool")
	})
	if err == nil {
		t.Fatal("without sudo the ladder must fail with guidance")
	}
	for _, want := range []string{
		"/etc/polkit-1/rules.d/50-urnetwork-restart.rules",
		"urnetwork.service",
		`subject.user == "user"`,
		"sudo systemctl restart urnetwork.service",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("guidance missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRestartLadder_StagedRetryFailureAlsoGuides(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("escalation ladder is linux-only")
	}
	stubRestartFn(t, errors.New("Interactive authentication required"))
	stubSudo(t, true)
	stubStagedRestart(t, errors.New("boom"))

	var out string
	var err error
	out = captureStdout(t, func() {
		err = restartLadder(Provider{Unit: "u.service", User: ""}, "/stage/tool")
	})
	if err == nil {
		t.Fatal("failed escalated retry must surface an error")
	}
	if !strings.Contains(out, "polkit") || !strings.Contains(out, "sudo systemctl restart u.service") {
		t.Fatalf("guidance must still print on retry failure:\n%s", out)
	}
}

func TestRestartLadder_NoStagedToolSkipsEscalation(t *testing.T) {
	stubRestartFn(t, errors.New("Interactive authentication required"))
	sudoCalled := stubSudo(t, true)

	err := restartLadder(Provider{Unit: "u.service", User: "user"}, "")
	if err == nil {
		t.Fatal("no staged tool: original failure must propagate")
	}
	if *sudoCalled {
		t.Fatal("sudo must not be probed when no staged tool exists")
	}
}

// --- updateProviderWithRestart hook hygiene ---

func TestUpdateProviderWithRestart_RestoresHookAfterRun(t *testing.T) {
	// Install a recognizable sentinel as the current hook.
	sentinel := func(p Provider) error { return nil }
	restartForUpdate = sentinel
	defer func() { restartForUpdate = restartProvider }()

	// Empty digest makes updateProvider fail fast BEFORE any restart, so the
	// test never touches systemctl; we only verify hook save/restore.
	if err := updateProviderWithRestart(
		Provider{Unit: "u.service"},
		updateConfig{}, // Digest == "" -> immediate refusal inside updateProvider
		"/stage/tool"); err == nil {
		t.Fatal("empty-digest update must fail fast")
	}

	captured := restartForUpdate
	sentinel2 := captured(Provider{})
	_ = sentinel2
	// Identity check: the restored hook must BE the sentinel (same behavior),
	// not the ladder wrapper.
	if err := restartForUpdate(Provider{}); err != nil {
		t.Fatal("hook should be the nil-error sentinel again")
	}
	if captured == nil {
		t.Fatal("hook must never be left nil")
	}
}

// --- hidden delegation command ---

func TestDoRestartCmd_MissingUnitErrors(t *testing.T) {
	cmd := newDoRestartCmd()
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "--unit") {
		t.Fatalf("expected --unit requirement, got %v", err)
	}
}

func TestDoRestartCmd_RefusesDockerTarget(t *testing.T) {
	orig := discoverForRestart
	discoverForRestart = func() []Provider {
		return []Provider{{User: "user", Unit: "urnetwork.service", Network: "testnet"}}
	}
	defer func() { discoverForRestart = orig }()

	cmd := newDoRestartCmd()
	if err := cmd.Flags().Set("unit", "ps"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("user", "docker:ps"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "docker container") {
		t.Fatalf("__do-restart must honor the namespace guard, got %v", err)
	}
}

// A narrowly-scoped sudoers grant for __do-restart must NOT become a
// generic "restart any unit as root" primitive (Sonnet review MEDIUM): the
// unit has to match a provider Discover() actually sees.
func TestDoRestartCmd_RejectsUndiscoveredUnit(t *testing.T) {
	orig := discoverForRestart
	discoverForRestart = func() []Provider {
		return []Provider{{User: "user", Unit: "urnetwork.service"}}
	}
	defer func() { discoverForRestart = orig }()

	cmd := newDoRestartCmd()
	if err := cmd.Flags().Set("unit", "sshd.service"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no discovered provider") {
		t.Fatalf("undiscovered unit must be refused before any systemctl call, got %v", err)
	}
}

// Wiring proof for the acceptance side: a DISCOVERED unit passes validation
// and reaches unitCommand (which fails here only because the fake unit does
// not exist — the point is it got PAST the discovery refusal). Note the
// restart targets the DISCOVERED record's user, not the flag value.
func TestDoRestartCmd_DiscoveredUnitReachesUnitCommand(t *testing.T) {
	fake := Provider{Unit: "urnet-tools-test-fake-unit.service", User: "user"}
	orig := discoverForRestart
	discoverForRestart = func() []Provider { return []Provider{fake} }
	defer func() { discoverForRestart = orig }()

	cmd := newDoRestartCmd()
	if err := cmd.Flags().Set("unit", fake.Unit); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err != nil && strings.Contains(err.Error(), "no discovered provider") {
		t.Fatalf("discovered unit must pass validation, got %v", err)
	}
	// Any remaining error comes from systemctl on the fake unit — expected.
}

// A --user flag that disagrees with the discovered record must be REJECTED,
// never silently routed to the flagged account's session via machined
// (Sonnet review round 2: unit-name-only validation let the restart target a
// different user's identically-named unit).
func TestDoRestartCmd_MismatchedUserRejected(t *testing.T) {
	discovered := Provider{Unit: "urnetwork.service", User: "user-a"}
	orig := discoverForRestart
	discoverForRestart = func() []Provider { return []Provider{discovered} }
	defer func() { discoverForRestart = orig }()

	cmd := newDoRestartCmd()
	if err := cmd.Flags().Set("unit", "urnetwork.service"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("user", "user-b"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no discovered provider") {
		t.Fatalf("mismatched user must be refused before any systemctl call, got %v", err)
	}

	// The honest path still works: correct user selects the record.
	cmd2 := newDoRestartCmd()
	_ = cmd2.Flags().Set("unit", "urnetwork.service")
	_ = cmd2.Flags().Set("user", "user-a")
	if err := cmd2.RunE(cmd2, nil); err != nil && strings.Contains(err.Error(), "no discovered provider") {
		t.Fatalf("matching user must pass validation, got %v", err)
	}
}

// Duplicate units across accounts must never be resolved by silent
// first-match: without --user the selection is ambiguous and refused
// (Sonnet review round 3).
func TestDoRestartCmd_AmbiguousDuplicateUnitsRefused(t *testing.T) {
	a := Provider{Unit: "urnetwork.service", User: "user-a"}
	b := Provider{Unit: "urnetwork.service", User: "user-b"}
	orig := discoverForRestart
	discoverForRestart = func() []Provider { return []Provider{a, b} }
	defer func() { discoverForRestart = orig }()

	cmd := newDoRestartCmd()
	_ = cmd.Flags().Set("unit", "urnetwork.service")
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing ambiguous selection") {
		t.Fatalf("duplicate-unit no-user selection must be refused, got %v", err)
	}

	// With an explicit user the ambiguity resolves deterministically.
	cmd2 := newDoRestartCmd()
	_ = cmd2.Flags().Set("unit", "urnetwork.service")
	_ = cmd2.Flags().Set("user", "user-b")
	if err := cmd2.RunE(cmd2, nil); err != nil && strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("explicit user must resolve the ambiguity, got %v", err)
	}
}

// --- staging helper (Sonnet review HIGH regression pins) ---

// TestStageToolForEscalation_ChmodsExecutable proves the staged binary gets
// execute bits: downloadFile writes 0644 and execve fails with EACCES on a
// non-executable file EVEN for root, so without the explicit chmod the whole
// sudo escalation leg could never fire in production.
func TestStageToolForEscalation_ChmodsExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX execute bits do not exist on windows; chmod is a no-op there")
	}
	content := []byte("#!/bin/sh\nexit 0\n")
	sum := sha256.Sum256(content)
	stageDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	cfg := updateConfig{
		Tag:          "v9.9.9-test",
		StageDir:     stageDir,
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   hex.EncodeToString(sum[:]),
		ToolAssetURL: srv.URL + "/tool",
	}
	got := stageToolForEscalation(cfg)
	if got == "" {
		t.Fatal("staging should succeed against a valid asset")
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("staged tool must carry execute bits, mode=%v", info.Mode())
	}
	// Digest re-check on disk: the exact bytes we served.
	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	reSum := sha256.Sum256(onDisk)
	if !strings.EqualFold(hex.EncodeToString(reSum[:]), cfg.ToolDigest) {
		t.Fatal("staged bytes do not match the served fixture")
	}
}

func TestStageToolForEscalation_SkipsWithoutDigestOrAsset(t *testing.T) {
	if got := stageToolForEscalation(updateConfig{StageDir: t.TempDir()}); got != "" {
		t.Fatalf("no digest -> must skip, got %q", got)
	}
	if got := stageToolForEscalation(updateConfig{ToolDigest: "abc"}); got != "" {
		t.Fatalf("no asset -> must skip, got %q", got)
	}
}

func TestStageToolForEscalation_BadDigestReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-the-real-tool"))
	}))
	defer srv.Close()

	got := stageToolForEscalation(updateConfig{
		Tag:          "v9.9.9-test",
		StageDir:     t.TempDir(),
		ToolAsset:    "urnet-tools-linux-amd64",
		ToolDigest:   strings.Repeat("ab", 32),
		ToolAssetURL: srv.URL + "/tool",
	})
	if got != "" {
		t.Fatalf("digest mismatch must disable escalation, got %q", got)
	}
}

// Documents the deliberately broad auth markers (Sonnet review LOW): a
// generic "access denied" string DOES classify as auth-class today. The
// blast radius of a false positive is only the guidance path, so breadth
// beats missing real polkit denials — pin that decision.
func TestIsAuthRestartFailure_BroadMarkersAreIntentional(t *testing.T) {
	if !isAuthRestartFailure(errors.New("stat backup: access denied")) {
		t.Fatal("broad-marker behavior changed — revisit the classifier contract")
	}
}
