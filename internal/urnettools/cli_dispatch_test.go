package urnettools

import (
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
		{"optimize"},
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
	for _, cmd := range []string{"providers", "list", "ps", "status", "exec", "restart", "logs"} {
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
		{"restart"},
		{"logs"},
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
}
