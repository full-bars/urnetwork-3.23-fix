package urnettools

import (
	"os/exec"
	"strings"
	"testing"
)

// shellWords splits a printed command the way a POSIX shell would.
func shellWords(t *testing.T, command string) []string {
	t.Helper()
	out, err := exec.Command("sh", "-c", "printf '%s\\0' "+command).Output()
	if err != nil {
		t.Fatalf("shell split of %q: %v", command, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// The command printed for a manual sudo re-run must pin the provider the
// parent resolved. If root can only see another user's provider of the same
// unit name, the re-run must be refused instead of configuring that binary.
func TestExtenderSetupManualSudoCommandPinsIdentity(t *testing.T) {
	oldDiscover, oldPriv, oldTTY := setupDiscoverFn, isPrivileged, stdinIsInteractiveOverride
	defer func() { setupDiscoverFn = oldDiscover; isPrivileged = oldPriv; stdinIsInteractiveOverride = oldTTY }()
	isPrivileged = func() bool { return false }
	stdinIsInteractiveOverride = func() bool { return false }
	parent := Provider{Unit: "urnetwork.service", User: currentUserName(), Binary: "/home/a/bin/provider"}
	setupDiscoverFn = func() []Provider { return []Provider{parent} }

	opts := setupRegressionOptions()
	opts.Argv = []string{"extender", "setup", "--yes", "--route=capability"}
	opts.NonInteractive = true
	opts.LookPath = func(string) (string, error) { return "sudo", nil }
	err := RunExtenderSetup(opts)
	if err == nil || !strings.Contains(err.Error(), "run: ") {
		t.Fatalf("expected a manual command in the error, got %v", err)
	}
	manual := err.Error()[strings.Index(err.Error(), "run: ")+len("run: "):]

	words := shellWords(t, manual)
	var pinned string
	for _, w := range words {
		if v, ok := strings.CutPrefix(w, urnetExtenderSetupTargetEnv+"="); ok {
			pinned = v
		}
	}
	if pinned != setupTargetIdentity([]Provider{parent}) {
		t.Fatalf("manual command lacks the pinned identity; words=%q", words)
	}
	if words[0] != "sudo" || words[len(words)-2] != "--unit" || words[len(words)-1] != "urnetwork.service" {
		t.Fatalf("manual command lost sudo or its selector: %q", words)
	}

	// Root runs that command and sees only user B's unit of the same name.
	t.Setenv(urnetExtenderSetupTargetEnv, pinned)
	isPrivileged = func() bool { return true }
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "userb", Binary: "/home/b/bin/provider"}}
	}
	rootOpts := setupRegressionOptions()
	rootOpts.IsRoot = func() bool { return true }
	rootOpts.Target = Target{Unit: "urnetwork.service"}
	cfg := &recordingConfigurer{mockPlatformConfigurer: mockPlatformConfigurer{platform: "linux"}}
	rootOpts.PlatformCfg = cfg
	err = RunExtenderSetup(rootOpts)
	if err == nil || !strings.Contains(err.Error(), "target changed") || len(cfg.applied) != 0 {
		t.Fatalf("root must refuse another owner's unit: err=%v applied=%v", err, cfg.applied)
	}
}

func TestExtenderSetupManualSudoCommandQuotesIdentity(t *testing.T) {
	odd := Provider{Unit: "it's a.service", User: "o'brien", Binary: "/opt/my dir/provider"}
	target := setupTargetIdentity([]Provider{odd})
	words := shellWords(t, formatSetupSudoCommand(ExtenderSetupOptions{Argv: []string{"extender", "setup", "--unit", odd.Unit}}, target))
	if len(words) < 6 || words[0] != "sudo" || words[1] != "--" || words[2] != "/usr/bin/env" || words[3] != urnetExtenderSetupTargetEnv+"="+target {
		t.Fatalf("identity did not survive shell quoting: %q", words)
	}
	if words[len(words)-1] != odd.Unit {
		t.Fatalf("selector lost: %q", words)
	}
}
