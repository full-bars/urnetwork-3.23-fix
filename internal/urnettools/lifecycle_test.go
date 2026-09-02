package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOptimizeLinuxRootCheck: optimizeLinux must refuse non-root with an
// actionable error when sudo is unavailable before touching sysctl.
func TestOptimizeLinuxRootCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the non-root guard cannot be exercised")
	}
	t.Setenv("PATH", "")
	err := optimizeLinux()
	if err == nil {
		t.Fatal("optimizeLinux on non-root without sudo must return an error")
	}
	if !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("error must say root is required, got: %v", err)
	}
}

// TestCmdOptimizeNoProviderRequired: optimize is host-wide sysctl/registry
// tuning and MUST succeed without requiring any discovered provider units
// or running processes on the box (regression test for "no providers found").
func TestCmdOptimizeNoProviderRequired(t *testing.T) {
	// Full CLI Run dispatch with dry-run flag.
	if err := Run([]string{"optimize", "-n"}); err != nil {
		t.Errorf("Run([optimize -n]) failed: %v", err)
	}
	// Direct cmdOptimize with dryRun=true and target flags ignored.
	if err := cmdOptimize([]string{"--unit", "foo"}, false, true); err != nil {
		t.Errorf("cmdOptimize(--unit foo, dryRun=true) failed: %v", err)
	}
	// Unexpected positional args must error.
	if err := cmdOptimize([]string{"invalid-extra-arg"}, true, false); err == nil {
		t.Error("cmdOptimize with unexpected positional arg should error")
	}
}

// TestWriteDropinEnvRoundTrip was deleted — it used os.WriteFile/readFile
// directly and exercised no production code. TestWriteDropinEnvMergeSameKeyReplace
// already covers the merge logic.

// TestTimerCalendarRewrite validates the OnCalendar substitution logic.
func TestTimerCalendarRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork-native-update.timer")
	orig := "[Unit]\nDescription=Run UrNetwork Native Update Weekly\n[Timer]\nOnCalendar=Sun *-*-* 00:00:00 UTC\nPersistent=true\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "OnCalendar=") {
			lines[i] = "OnCalendar=daily"
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("OnCalendar not replaced")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "OnCalendar=daily") {
		t.Errorf("timer rewrite failed: %s", out)
	}
	if strings.Contains(string(out), "Sun *-*-* 00:00:00 UTC") {
		t.Errorf("old calendar still present: %s", out)
	}
}

// TestCmdTuneModeValidation: tuning commands require a mode argument
// (deterministic error without providers).
func TestCmdTuneModeValidation(t *testing.T) {
	err := cmdTune("turbo", []string{}, false, false)
	if err == nil {
		t.Fatal("expected error for turbo with no mode")
	}
	if !contains(err.Error(), "requires a mode") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdHubRequiresSubcommand: hub with no subcommand errors cleanly.
func TestCmdHubRequiresSubcommand(t *testing.T) {
	err := cmdHub([]string{}, false, false)
	if err == nil {
		t.Fatal("expected error for hub with no subcommand")
	}
	if !contains(err.Error(), "requires a subcommand") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdProxyRequiresSubcommand: proxy with no subcommand errors cleanly.
func TestCmdProxyRequiresSubcommand(t *testing.T) {
	err := cmdProxy([]string{}, false, false)
	if err == nil {
		t.Fatal("expected error for proxy with no subcommand")
	}
	if !contains(err.Error(), "requires a subcommand") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestHubURLValidation was deleted — it called strings.HasPrefix on a local
// variable and never exercised any production code.

// TestWriteDropinEnvMergeSameKeyReplace: writing a drop-in with the same
// environment key replaces the old value and keeps different keys.
func TestWriteDropinEnvMergeSameKeyReplace(t *testing.T) {
	dir := t.TempDir()
	// Create an existing drop-in with two env lines.
	existing := "[Service]\nEnvironment=\"URNETWORK_PROFILE=eco\"\nEnvironment=\"URNETWORK_RAMLOGS=1\"\n"
	path := filepath.Join(dir, "tuning.conf")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	// Call the PRODUCTION merge helper — not a copy of its logic
	// (coderabbit major: reimplemented tests cannot detect regressions).
	got, err := mergeDropinEnvFile(path, "URNETWORK_PROFILE=turbo-v4")
	if err != nil {
		t.Fatal(err)
	}
	// The old URNETWORK_PROFILE=eco should be gone.
	if strings.Contains(got, "eco") {
		t.Errorf("same-key replace failed: old value 'eco' still present: %s", got)
	}
	// The new URNETWORK_PROFILE=turbo-v4 should be there.
	if !strings.Contains(got, "turbo-v4") {
		t.Errorf("new value 'turbo-v4' missing: %s", got)
	}
	// URNETWORK_RAMLOGS=1 (different key) must be preserved.
	if !strings.Contains(got, "URNETWORK_RAMLOGS=1") {
		t.Errorf("different key 'URNETWORK_RAMLOGS' was dropped: %s", got)
	}
	// Exactly one [Service] header (free-review LOW: duplicate header bug).
	if n := strings.Count(got, "[Service]"); n != 1 {
		t.Errorf("expected exactly one [Service] header, got %d:\n%s", n, got)
	}
}

// TestCmdUninstallPathGuards: cmdUninstall must not remove "/" or paths with
// degenerate basenames (. or /). Calls the PRODUCTION safeRemoveTarget guard
// (coderabbit major: reimplemented tests cannot detect regressions).
func TestCmdUninstallPathGuards(t *testing.T) {
	// These must be REJECTED (guard returns false). "/" and "/./" are
	// Unix-root forms; on Windows the root is a drive path, so those are
	// not roots there and the guard legitimately treats them differently.
	// Windows-relevant rejects: drive roots, empty, relative.
	rejectedBins := []string{".", "", "relative/path"}
	if runtime.GOOS != "windows" {
		rejectedBins = append(rejectedBins, "/", "/./")
	} else {
		rejectedBins = append(rejectedBins, `C:\`, `\\?\C:\`)
	}
	for _, bin := range rejectedBins {
		if safeRemoveTarget(bin) {
			t.Errorf("path %q should be rejected by guards but would pass", bin)
		}
	}
	// These must PASS the guard (guard returns true). Paths are
	// platform-appropriate. Bare "provider" is relative and correctly
	// rejected, so it is not in this list.
	acceptedBins := []string{}
	if runtime.GOOS != "windows" {
		acceptedBins = append(acceptedBins,
			"/provider",
			"/home/urnet/.local/share/urnetwork-provider/bin/urnetwork",
			"/usr/local/bin/provider")
	} else {
		acceptedBins = append(acceptedBins, `C:\Program Files\urnetwork\provider.exe`)
	}
	for _, bin := range acceptedBins {
		if !safeRemoveTarget(bin) {
			t.Errorf("binary %q should pass guards but was rejected", bin)
		}
	}
	// State dir guards: empty is rejected on all platforms; "/" only on Unix.
	if safeRemoveTarget("") {
		t.Errorf("state dir '' should be rejected")
	}
	if runtime.GOOS != "windows" && safeRemoveTarget("/") {
		t.Errorf("state dir '/' should be rejected")
	}
	// Valid state dir passes (platform-appropriate path).
	stateDir := "/home/urnet/.urnetwork"
	if runtime.GOOS == "windows" {
		stateDir = `C:\Users\urnet\.urnetwork`
	}
	if !safeRemoveTarget(stateDir) {
		t.Errorf("state dir %q should pass", stateDir)
	}
}

// TestUnitCommandArgv: unitCommandArgs must produce the correct argv for
// both system and user units. Calls the PRODUCTION argv builder directly
// (coderabbit major: reimplemented tests cannot detect regressions).
func TestUnitCommandArgv(t *testing.T) {
	// A fake unit name that won't exist on any box -> isUserUnit = true.
	userUnit := "urnet-tools-test-fake-unit-argv.service"
	if !isUserUnit(userUnit) {
		t.Skip("isUserUnit returned false for fake unit; cannot test user-level argv")
	}
	// Same-user unit: systemctl --user <action> <unit> [extra...].
	got := unitCommandArgs(Provider{Unit: userUnit, User: currentUserName()}, "restart", "--no-block")
	want := []string{"systemctl", "--user", "restart", userUnit, "--no-block"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("same-user argv = %v, want %v", got, want)
	}
	// Cross-user unit: systemctl --user -M <user>@ <action> <unit> [extra...].
	got = unitCommandArgs(Provider{Unit: userUnit, User: "other-user-9f3a"}, "restart", "--no-block")
	want = []string{"systemctl", "--user", "-M", "other-user-9f3a@", "restart", userUnit, "--no-block"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("cross-user argv = %v, want %v", got, want)
	}
	// System-level unit: systemctl <action> <unit>.
	got = unitCommandArgs(Provider{Unit: "urnetwork-native.service", User: ""}, "start")
	want = []string{"systemctl", "start", "urnetwork-native.service"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("system-unit argv = %v, want %v", got, want)
	}
	// No unit: systemctl <action> alone (caller will error on the empty unit).
	got = unitCommandArgs(Provider{}, "restart")
	want = []string{"systemctl", "restart"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("empty-unit argv = %v, want %v", got, want)
	}
}

// TestWriteTimerCalendarCreatesMissingFile: auto-update on a fresh install
// (no pre-existing timer file) must CREATE the unit file, not error with
// "read timer ...: no such file". Mirrors the shell wrapper's install-time
// creation. Regression for the strict unix-lifecycle CI assert.
func TestWriteTimerCalendarCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork-update.timer")
	content := "[Unit]\nDescription=Run URnetwork Update\n\n[Timer]\nOnCalendar=Sun *-*-* 00:00:00 UTC\nPersistent=true\n\n[Install]\nWantedBy=default.target\n"
	if err := writeTimerUnitAtomic(path, content); err != nil {
		t.Fatalf("writeTimerUnitAtomic: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "OnCalendar=Sun *-*-* 00:00:00 UTC") {
		t.Fatalf("created timer missing OnCalendar: %s", b)
	}
	if !strings.Contains(string(b), "WantedBy=default.target") {
		t.Fatalf("created timer missing [Install] section: %s", b)
	}
}

// TestWriteTimerUnitAtomicNoPartialOnCrash: writeTimerUnitAtomic must not
// leave a half-written unit at the target path — the temp file is renamed,
// so a crash mid-write leaves the OLD file intact (or nothing), never a
// truncated unit.
func TestWriteTimerUnitAtomicNoPartialOnCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork-update.timer")
	if err := writeTimerUnitAtomic(path, "first"); err != nil {
		t.Fatal(err)
	}
	if err := writeTimerUnitAtomic(path, "second"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "second" {
		t.Fatalf("after rename, content = %q, want %q", b, "second")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after rename: %v", err)
	}
}

// TestConsumeDockerBareTarget: a leading bare positional that matches a
// discovered container becomes the target (usage text documents `[target]`),
// while a proxy file path or URL is left untouched. Flags before the name
// (e.g. --force) are skipped.
func TestConsumeDockerBareTarget(t *testing.T) {
	providers := []Provider{
		{Unit: "urnet-test"},
		{Unit: "urfix-auto"},
	}
	cases := []struct {
		name     string
		rest     []string
		wantUnit string
		wantRest []string
	}{
		{"bare name consumed", []string{"urnet-test", "5"}, "urnet-test", []string{"5"}},
		{"flag skipped then name consumed", []string{"--force", "urnet-test"}, "urnet-test", []string{"--force"}},
		{"file path untouched", []string{"/tmp/proxies.txt"}, "", []string{"/tmp/proxies.txt"}},
		{"url untouched", []string{"https://example.com/list.txt"}, "", []string{"https://example.com/list.txt"}},
		{"no match untouched", []string{"5"}, "", []string{"5"}},
		{"already targeted untouched", []string{"--unit", "urfix-auto", "3"}, "urfix-auto", []string{"--unit", "urfix-auto", "3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tgt Target
			if c.name == "already targeted untouched" {
				tgt.Unit = "urfix-auto"
			}
			gotT, gotRest := consumeDockerBareTarget(providers, tgt, c.rest)
			if gotT.Unit != c.wantUnit {
				t.Fatalf("Unit = %q, want %q (rest %v)", gotT.Unit, c.wantUnit, c.rest)
			}
			if len(gotRest) != len(c.wantRest) {
				t.Fatalf("rest = %v, want %v", gotRest, c.wantRest)
			}
			for i := range gotRest {
				if gotRest[i] != c.wantRest[i] {
					t.Fatalf("rest = %v, want %v", gotRest, c.wantRest)
				}
			}
		})
	}
}
