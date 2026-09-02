package urnettools

import (
	"strings"
	"testing"
)

// Regression tests for the Cobra routing migration. They guard the invariants
// that the migration was supposed to preserve: the full command surface still
// dispatches to the existing handlers, aliases still resolve, help never
// executes, and the commands that own their own help keep it.

// TestRunSessionOwnHelp: `session --help` must print the RICH cmdSession usage
// (save/load + --allow-different-account), NOT a Cobra stub. Guards the review
// finding that newCobraCmd's broad help interception was hiding it.
func TestRunSessionOwnHelp(t *testing.T) {
	out := captureStderr(t, func() {
		if err := Run([]string{"session", "--help"}); err != nil {
			t.Fatalf("Run([session --help]) = %v, want nil", err)
		}
	})
	for _, want := range []string{"session save <file>", "session load <file>", "--allow-different-account"} {
		if !strings.Contains(out, want) {
			t.Errorf("session --help missing %q; got: %s", want, out)
		}
	}
}

// TestRunSelfHealHelpNoError: `self-heal --help` must return nil (its own help),
// not error or block.
func TestRunSelfHealHelpNoError(t *testing.T) {
	for _, cmd := range []string{"self-heal", "selfheal"} {
		if err := Run([]string{cmd, "--help"}); err != nil {
			t.Errorf("Run([%s --help]) = %v, want nil", cmd, err)
		}
	}
}

// TestRunVersionForms: version, --version, and -v all print the version at top
// level (the aliases on the version command are dead in Cobra; the special case
// in Run handles these).
func TestRunVersionForms(t *testing.T) {
	for _, arg := range []string{"--version", "-v"} {
		out := captureStdout(t, func() {
			if err := Run([]string{arg}); err != nil {
				t.Errorf("Run([%s]) = %v, want nil", arg, err)
			}
		})
		if strings.TrimSpace(out) != ToolVersion {
			t.Errorf("Run([%s]) = %q, want %q", arg, strings.TrimSpace(out), ToolVersion)
		}
	}
	// `version` subcommand shows tool version as first line (richer output).
	out := captureStdout(t, func() {
		if err := Run([]string{"version"}); err != nil {
			t.Errorf("Run([version]) = %v, want nil", err)
		}
	})
	if !strings.Contains(out, ToolVersion) {
		t.Errorf("Run([version]) = %q, want it to contain %q", out, ToolVersion)
	}
}

// TestRunDockerProxyBareExitsZero: bare `urnet-docker proxy` lists subcommands
// and returns nil (exit 0), restoring the pre-migration behavior (was exit 1).
func TestRunDockerProxyBareExitsZero(t *testing.T) {
	out := captureStderr(t, func() {
		if err := RunDocker([]string{"proxy"}); err != nil {
			t.Fatalf("RunDocker([proxy]) = %v, want nil (exit 0)", err)
		}
	})
	// Bare `proxy` prints the full docker usage (pre-cobra behavior) and exits 0.
	if !strings.Contains(out, "urnet-docker") || !strings.Contains(strings.ToLower(out), "proxy") {
		t.Errorf("RunDocker([proxy]) stderr = %q, want docker usage", out)
	}
}

// TestCobraUnknownCommandErrors: an unknown command must error (not silently
// succeed), so typos are caught rather than swallowed by the dispatcher.
func TestCobraUnknownCommandErrors(t *testing.T) {
	for _, args := range [][]string{{"nonesuch"}, {"definitly-not-a-cmd"}} {
		if err := Run(args); err == nil {
			t.Errorf("Run(%v) = nil, want unknown-command error", args)
		}
	}
}

// TestRunVersionWithTrailingArg: `-v`/`--version`/`version` with trailing args
// still print the version, matching the pre-cobra dispatcher (which matched on
// args[0] only).
func TestRunVersionWithTrailingArg(t *testing.T) {
	for _, arg := range []string{"-v", "--version"} {
		out := captureStdout(t, func() {
			if err := Run([]string{arg, "junk"}); err != nil {
				t.Errorf("Run([%s junk]) = %v, want nil", arg, err)
			}
		})
		if strings.TrimSpace(out) != ToolVersion {
			t.Errorf("Run([%s junk]) = %q, want %q", arg, strings.TrimSpace(out), ToolVersion)
		}
	}
	// `version` with trailing arg still produces richer output (just check
	// it contains the tool version and doesn't error).
	out := captureStdout(t, func() {
		if err := Run([]string{"version", "junk"}); err != nil {
			t.Errorf("Run([version junk]) = %v, want nil", err)
		}
	})
	if !strings.Contains(out, ToolVersion) {
		t.Errorf("Run([version junk]) = %q, want it to contain %q", out, ToolVersion)
	}
}
