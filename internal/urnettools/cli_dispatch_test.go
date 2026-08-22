package urnettools

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestRunHelpEveryCommand: -h must be safe (prints help, never executes) on
// every subcommand Run() dispatches to, including every alias. This actually
// invokes Run() directly — earlier coverage (TestDispatchHelpIsSafe) only
// exercised the underlying parser and left Run() itself at 0% coverage.
func TestRunHelpEveryCommand(t *testing.T) {
	cmds := []string{
		"providers", "list", "ps",
		"status", "update", "proxy",
		"summary", "report", "hot-restart", "hotrestart",
		"start", "stop", "restart", "logs", "hub",
		"turbo", "eco", "lowmode", "ramlogs", "auto",
		"optimize", "auto-start", "autostart", "auto-update", "autoupdate",
		"uninstall", "reinstall",
		// PR #438: self-heal and its alias are dispatched by Run() but
		// were not in this table; a regression adding -h that errors would
		// have gone unnoticed.
		"self-heal", "selfheal",
	}
	for _, cmd := range cmds {
		for _, flag := range []string{"-h", "--help"} {
			if err := Run([]string{cmd, flag}); err != nil {
				t.Errorf("Run([%q, %q]) = %v, want nil (help must never execute)", cmd, flag, err)
			}
		}
	}
	for _, args := range [][]string{nil, {}, {"help"}, {"-h"}, {"--help"}} {
		if err := Run(args); err != nil {
			t.Errorf("Run(%v) = %v, want nil", args, err)
		}
	}
}

// TestRunUnknownCommand: an unrecognized subcommand must error, not panic or
// silently no-op.
func TestRunUnknownCommand(t *testing.T) {
	err := Run([]string{"frobnicate"})
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRunNoProvidersOnBox: on a box with zero discoverable providers (this
// test sandbox has none), every targeting command must refuse with a clear
// "no providers found" error rather than panicking or silently acting. This
// exercises the full Run() -> parseGlobalFlags -> cmdXxx -> Discover() ->
// selectTarget dispatch chain for each command.
func TestRunNoProvidersOnBox(t *testing.T) {
	if len(Discover()) != 0 {
		t.Skip("this test requires a box with zero discoverable providers")
	}
	cmds := [][]string{
		{"status"},
		{"summary"},
		{"report"},
		{"hot-restart"},
		{"start"},
		{"stop"},
		{"restart"},
		{"logs"},
		{"uninstall"},
		{"reinstall"},
	}
	for _, args := range cmds {
		err := Run(args)
		if err == nil {
			t.Errorf("Run(%v) with no providers = nil, want an error", args)
			continue
		}
		if !strings.Contains(err.Error(), "no providers found") {
			t.Errorf("Run(%v) = %v, want \"no providers found\"", args, err)
		}
	}
}

// TestRunUpdateForceNoProviders: cmdUpdate's provider-selection step must
// run (and refuse) BEFORE any release lookup or staging occurs. -f is
// required here because plain `update` with no target goes through the
// interactive picker when stdin looks like a terminal (forceInteractive),
// which would otherwise block reading a selection that will never come.
func TestRunUpdateForceNoProviders(t *testing.T) {
	if len(Discover()) != 0 {
		t.Skip("this test requires a box with zero discoverable providers")
	}
	err := Run([]string{"update", "-f"})
	if err == nil || !strings.Contains(err.Error(), "no providers found") {
		t.Errorf("Run([update -f]) = %v, want \"no providers found\"", err)
	}
}

// TestRunProvidersEmptyBox: with zero providers, `providers`/`list`/`ps` must
// print an informational message and return nil, not error.
func TestRunProvidersEmptyBox(t *testing.T) {
	if len(Discover()) != 0 {
		t.Skip("this test requires a box with zero discoverable providers")
	}
	for _, cmd := range []string{"providers", "list", "ps"} {
		if err := Run([]string{cmd}); err != nil {
			t.Errorf("Run([%q]) = %v, want nil", cmd, err)
		}
	}
}

// TestRunProxyNoSubcommand: `proxy` with no further args must error before
// any targeting happens.
func TestRunProxyNoSubcommand(t *testing.T) {
	err := Run([]string{"proxy"})
	if err == nil || !strings.Contains(err.Error(), "requires a subcommand") {
		t.Errorf("Run([proxy]) = %v, want \"requires a subcommand\"", err)
	}
}

// TestRunTuneRequiresMode: turbo/eco/lowmode/ramlogs/auto all require a mode
// argument, validated before targeting.
func TestRunTuneRequiresMode(t *testing.T) {
	for _, cmd := range []string{"turbo", "eco", "lowmode", "ramlogs", "auto"} {
		err := Run([]string{cmd})
		if err == nil || !strings.Contains(err.Error(), "requires a mode") {
			t.Errorf("Run([%q]) = %v, want \"requires a mode\"", cmd, err)
		}
	}
}

// TestRunAutoStartAutoUpdateRequireArgs: auto-start/auto-update (and their
// no-hyphen aliases) require an explicit mode/interval before targeting.
func TestRunAutoStartAutoUpdateRequireArgs(t *testing.T) {
	for _, cmd := range []string{"auto-start", "autostart"} {
		err := Run([]string{cmd})
		if err == nil || !strings.Contains(err.Error(), "on|off") {
			t.Errorf("Run([%q]) = %v, want \"on|off\"", cmd, err)
		}
	}
	for _, cmd := range []string{"auto-update", "autoupdate"} {
		err := Run([]string{cmd})
		if err == nil || !strings.Contains(err.Error(), "daily|weekly|monthly|off") {
			t.Errorf("Run([%q]) = %v, want \"daily|weekly|monthly|off\"", cmd, err)
		}
	}
}

// TestRunHubNoSubcommand: `hub` with no further args must error before any
// targeting happens.
func TestRunHubNoSubcommand(t *testing.T) {
	err := Run([]string{"hub"})
	if err == nil || !strings.Contains(err.Error(), "requires a subcommand") {
		t.Errorf("Run([hub]) = %v, want \"requires a subcommand\"", err)
	}
}

// TestRunUnknownFlagPropagates: an unknown --flag reaching the strict
// parseTargetFlags parser (via cmdStatus) must error, proving parseGlobalFlags
// correctly leaves non-global flags in rest for the subcommand parser.
func TestRunUnknownFlagPropagates(t *testing.T) {
	err := Run([]string{"status", "--bogus-flag"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("Run([status --bogus-flag]) = %v, want \"unknown flag\"", err)
	}
}

// TestRunForceAndDryRunParsed: -f and -n must both be consumed by
// parseGlobalFlags without leaking into the subcommand's positional args
// (which would otherwise be misinterpreted as a target token).
func TestRunForceAndDryRunParsed(t *testing.T) {
	if len(Discover()) != 0 {
		t.Skip("this test requires a box with zero discoverable providers")
	}
	// -f alone with zero providers still hits "no providers found" (force
	// only skips the confirm prompt, never provider resolution).
	err := Run([]string{"restart", "-f", "-n"})
	if err == nil || !strings.Contains(err.Error(), "no providers found") {
		t.Errorf("Run([restart -f -n]) = %v, want \"no providers found\"", err)
	}
}

// TestRunDockerHelpEveryCommand mirrors TestRunHelpEveryCommand for the
// urnet-docker entry point (RunDocker), which had 0% coverage.
func TestRunDockerHelpEveryCommand(t *testing.T) {
	for _, cmd := range []string{
		"providers", "list", "ps", "status", "start", "stop", "restart", "logs",
		"auth", "choose-network", "choose_network", "summary", "report",
		"self-heal", "selfheal", "set", "fast-auth", "fastauth", "hub", "session",
		"proxy",
	} {
		for _, flag := range []string{"-h", "--help"} {
			if err := RunDocker([]string{cmd, flag}); err != nil {
				t.Errorf("RunDocker([%q, %q]) = %v, want nil", cmd, flag, err)
			}
		}
	}
	for _, args := range [][]string{nil, {}, {"help"}, {"-h"}, {"--help"}} {
		if err := RunDocker(args); err != nil {
			t.Errorf("RunDocker(%v) = %v, want nil", args, err)
		}
	}
}

// TestRunDockerUnknownCommand mirrors TestRunUnknownCommand for RunDocker.
func TestRunDockerUnknownCommand(t *testing.T) {
	err := RunDocker([]string{"frobnicate"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("RunDocker([frobnicate]) = %v, want \"unknown command\"", err)
	}
}

// TestRunDockerNoContainers: with the docker CLI stubbed to a nonexistent
// binary (deterministic "no containers" without a real daemon), every
// targeting docker command must refuse cleanly.
func TestRunDockerNoContainers(t *testing.T) {
	t.Setenv("URNET_DOCKER_BIN", "urnet-tools-test-no-such-binary-9f3a")

	if err := RunDocker([]string{"providers"}); err != nil {
		t.Errorf("RunDocker([providers]) = %v, want nil (prints \"no provider containers found\")", err)
	}
	for _, args := range [][]string{
		{"status"},
		{"start"},
		{"stop"},
		{"restart"},
		{"logs"},
		{"auth"},
		{"choose-network", "a", "b"},
		{"summary"},
		{"self-heal"},
		{"set"},
		{"fast-auth"},
		{"hub"},
		{"session"},
	} {
		err := RunDocker(args)
		if err == nil || !strings.Contains(err.Error(), "no providers found") {
			t.Errorf("RunDocker(%v) = %v, want \"no providers found\"", args, err)
		}
	}
	// exec requires a command before targeting is attempted.
	err := RunDocker([]string{"exec"})
	if err == nil || !strings.Contains(err.Error(), "requires a command") {
		t.Errorf("RunDocker([exec]) = %v, want \"requires a command\"", err)
	}
	// exec with a command still refuses on zero containers.
	err = RunDocker([]string{"exec", "urnet-tools", "status"})
	if err == nil || !strings.Contains(err.Error(), "no providers found") {
		t.Errorf("RunDocker([exec urnet-tools status]) = %v, want \"no providers found\"", err)
	}
	// -- separator: everything after it is the container command (may
	// contain its own flags); an explicit --unit target on a box with no
	// containers errors as no-matching-provider (more precise than the
	// no-target "no providers found").
	err = RunDocker([]string{"exec", "--unit", "x", "--", "urnet-tools", "proxy", "add", "--proxy_file=/tmp/p.txt"})
	if err == nil || !strings.Contains(err.Error(), "matches no running provider") {
		t.Errorf("RunDocker([exec --unit x -- cmd...]) = %v, want \"matches no running provider\"", err)
	}
	// Unknown flag BEFORE the command must error loudly (was silently
	// dropped before the -- separator fix), with a hint to use --.
	err = RunDocker([]string{"exec", "--unit", "x", "--verbose", "urnet-tools"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "--") {
		t.Errorf("RunDocker([exec --unit x --verbose cmd]) = %v, want unknown-flag error with -- hint", err)
	}
	// Inner -f before the command: same refusal (must not be swallowed).
	err = RunDocker([]string{"exec", "--unit", "x", "-f", "urnet-tools"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("RunDocker([exec --unit x -f cmd]) = %v, want unknown-flag error", err)
	}
}

// captureStderr runs fn with os.Stderr redirected to a buffer and returns
// what was written. Usage/help output goes to stderr (cli.go usage(),
// cli_docker.go usageDocker()), so asserting on it proves WHICH usage was
// printed — a nil-error check alone lets a regression to the wrong tool's
// help pass (verified 2026-08-12 review).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	w.Close()
	<-done
	r.Close()
	return buf.String()
}

// TestCapturePipeLargeOutputNoDeadlock ensures captureStderr / captureStdout
// safely handle outputs much larger than the OS pipe buffer (e.g. 128KB)
// without deadlocking on Windows or Linux.
func TestCapturePipeLargeOutputNoDeadlock(t *testing.T) {
	largeData := strings.Repeat("0123456789abcdef\n", 8192) // 136 KB
	out := captureStderr(t, func() {
		_, _ = os.Stderr.WriteString(largeData)
	})
	if len(out) != len(largeData) {
		t.Errorf("captureStderr captured %d bytes, want %d", len(out), len(largeData))
	}
}

// TestRunSelfUpdateHelp: `self-update`/`selfupdate` (and their -h/--help)
// must be dispatched by Run() to cmdSelfUpdate and print the urnet-tools
// usage without touching the network or prompting.
func TestRunSelfUpdateHelp(t *testing.T) {
	for _, cmd := range []string{"self-update", "selfupdate"} {
		for _, flag := range []string{"-h", "--help"} {
			out := captureStderr(t, func() {
				if err := Run([]string{cmd, flag}); err != nil {
					t.Errorf("Run([%q, %q]) = %v, want nil", cmd, flag, err)
				}
			})
			if !strings.Contains(out, "update this tool binary itself") {
				t.Errorf("Run([%q %q]) stderr = %q, want command-specific usage", cmd, flag, out)
			}
		}
	}
}

// TestRunDockerUpdateHelp: RunDocker's `update`/`self-update`/`selfupdate`
// aliases all route to cmdSelfUpdate and must print the DOCKER usage — a
// regression to the shared parseGlobalFlags' urnet-tools usage must fail
// here (verified 2026-08-12 review).
func TestRunDockerUpdateHelp(t *testing.T) {
	for _, cmd := range []string{"update", "self-update", "selfupdate"} {
		for _, flag := range []string{"-h", "--help"} {
			out := captureStderr(t, func() {
				if err := RunDocker([]string{cmd, flag}); err != nil {
					t.Errorf("RunDocker([%q, %q]) = %v, want nil", cmd, flag, err)
				}
			})
			if !strings.Contains(out, "update urnet-docker binary on host") {
				t.Errorf("RunDocker([%q %q]) stderr = %q, want command-specific usage", cmd, flag, out)
			}
		}
	}
}

// TestRunSelfUpdateUnknownFlagPropagates: an unrecognized flag reaching
// cmdSelfUpdate via Run()'s dispatch must surface cmdSelfUpdate's OWN
// self-update-specific error ("for self-update"), proving parseGlobalFlags
// leaves subcommand-specific flags in rest for self-update just as it does
// for other subcommands — a generic "unknown flag" from the global parser
// would not prove the dispatch (verified 2026-08-12 review).
func TestRunSelfUpdateUnknownFlagPropagates(t *testing.T) {
	err := Run([]string{"self-update", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "for self-update") {
		t.Errorf("Run([self-update --bogus]) = %v, want self-update-specific unknown-flag error", err)
	}
}

// TestRunDockerSelfUpdateUnknownFlagPropagates mirrors the above for
// RunDocker's update/self-update/selfupdate aliases.
func TestRunDockerSelfUpdateUnknownFlagPropagates(t *testing.T) {
	for _, cmd := range []string{"update", "self-update", "selfupdate"} {
		err := RunDocker([]string{cmd, "--bogus"})
		if err == nil || !strings.Contains(err.Error(), "for self-update") {
			t.Errorf("RunDocker([%q --bogus]) = %v, want self-update-specific unknown-flag error", cmd, err)
		}
	}
}

func TestParseTargetFlagsEqualsForm(t *testing.T) {
	// The = form (--user=value) must be accepted identically to the space
	// form (--user value). Before this fix, parseTargetFlagsInner only matched
	// the space form, so "=" args survived into subcommand flag loops which
	// rejected them as "unknown flag" (e.g. update, whose own flag loop builds
	// on the leftover args). Regression test for the multi-provider targeting
	// failure on fleet boxes.
	cases := []struct {
		name  string
		args  []string
		wantU string
		wantN string
	}{
		{"space form", []string{"--user", "bob"}, "bob", ""},
		{"equals form", []string{"--user=bob"}, "bob", ""},
		{"network equals", []string{"--network=mesocyclone"}, "", "mesocyclone"},
		{"mixed", []string{"--user=alice", "extra"}, "alice", ""},
		{"state-dir equals", []string{"--state-dir=/srv/urnet"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t2, rest, err := parseTargetFlags(c.args)
			if err != nil {
				t.Fatalf("parseTargetFlags(%v): %v", c.args, err)
			}
			if t2.User != c.wantU {
				t.Fatalf("User = %q, want %q", t2.User, c.wantU)
			}
			if t2.Network != c.wantN {
				t.Fatalf("Network = %q, want %q", t2.Network, c.wantN)
			}
			if c.name == "state-dir equals" {
				if t2.StateDir != "/srv/urnet" {
					t.Fatalf("StateDir = %q, want /srv/urnet", t2.StateDir)
				}
			}
			if c.name == "mixed" {
				if len(rest) != 1 || rest[0] != "extra" {
					t.Fatalf("rest = %v, want [extra]", rest)
				}
			}
		})
	}
}

func TestParseTargetFlagsEqualsFormEmptyValue(t *testing.T) {
	if _, _, err := parseTargetFlags([]string{"--user="}); err == nil {
		t.Fatal("expected error for empty = value, got nil")
	}
}

// The space form must reject an empty value too (--user ""). Before setField
// rejected empty values, the parser returned a zero-value Target that on a
// single-provider host silently targeted the default provider instead of
// refusing the invalid selector.
func TestParseTargetFlagsSpaceFormEmptyValue(t *testing.T) {
	if _, _, err := parseTargetFlags([]string{"--user", ""}); err == nil {
		t.Fatal("expected error for empty space-separated value, got nil")
	}
}

// TestRunSelfHealNoProviderRequired: `self-heal status` must succeed (or at
// minimum not error with "no providers found") on a box with zero providers,
// because self-heal reads/writes ~/.urnetwork/proxy_self_heal — it has no
// dependency on Discover(). A regression that adds provider-selection to
// cmdSelfHeal would break self-heal on bare/docker boxes.
func TestRunSelfHealNoProviderRequired(t *testing.T) {
	if len(Discover()) != 0 {
		t.Skip("this test requires a box with zero discoverable providers")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	err := Run([]string{"self-heal", "status"})
	if err != nil && strings.Contains(err.Error(), "no providers found") {
		t.Errorf("Run([self-heal status]) = %v\n"+
			"REGRESSION: self-heal must not require provider discovery", err)
	}
}

// TestRunSelfHealInvalidArg: an unrecognized sub-arg to self-heal must return
// a clear error naming the invalid arg (not panic or silently no-op).
// Covers the default branch of cmdSelfHeal's switch.
func TestRunSelfHealInvalidArg(t *testing.T) {
	err := Run([]string{"self-heal", "bogus"})
	if err == nil {
		t.Fatal("Run([self-heal bogus]) = nil, want error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the invalid arg, got: %v", err)
	}
	if !strings.Contains(err.Error(), "on|off|status") {
		t.Errorf("error should list valid args (on|off|status), got: %v", err)
	}
}

// TestUsageContainsExpectedSections: pins the help output structure so a
// future refactor can't silently drop the sectioned layout introduced in
// PR #437. This covers both the emoji-decorated section headers and the
// section names that operators rely on when reading `urnet-tools help`.
func TestUsageContainsExpectedSections(t *testing.T) {
	out := captureStderr(t, func() { usage() })
	sections := []string{
		"urnet-tools",
		"Core Commands",
		"Performance",
		"Proxy Management",
		"Hub Management",
		"Maintenance",
		"Targeting rules",
		"Force",
		"-f, --force",
		"-n, --dry-run",
	}
	for _, sec := range sections {
		if !strings.Contains(out, sec) {
			t.Errorf("usage() output missing %q\nREGRESSION: help section dropped or renamed", sec)
		}
	}
}

// TestRunDockerExecHelpAfterSepNotIntercepted guards the CRITICAL review fix:
// `exec -- <cmd> --help` must forward the help after the separator to the
// container command, NOT be intercepted by the docker CLI's own help routing.
// The old dispatcher did not broad-scan hasHelpFlag for exec (help after '--'
// belongs to the container binary). The buggy Cobra migration re-introduced it
// and printed the docker exec help; the fix builds exec raw.
func TestRunDockerExecHelpAfterSepNotIntercepted(t *testing.T) {
	out := captureStderr(t, func() { _ = RunDocker([]string{"exec", "--", "urnet-tools", "proxy", "--help"}) })
	// If the docker CLI intercepted, it would print the exec command's own help
	// (its Short text) and return nil. Now it delegates, so that help text must
	// not appear, and we should not have silently returned success-as-help.
	if strings.Contains(out, "run arbitrary command inside container") {
		t.Fatalf("help after '--' was intercepted by the docker CLI: %q", out)
	}
}
