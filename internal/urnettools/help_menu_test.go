package urnettools

import (
	"strings"
	"testing"
)

// Regression tests for per-command help. Every command in both urnet-tools and
// urnet-docker must render its OWN help page on -h/--help (a per-command Usage
// line that names the command), never the top-level menu. This guards against
// a command silently falling back to the root usage screen or its help page
// erroring. The version command is the documented exception: -h on 'version'
// is intercepted at the top level and prints the tool version (see
// TestVersionHelpIsRootIntercepted).

// firstUsageLine returns the usage block: the "Usage:" header plus every
// consecutive non-empty line after it. Cobra renders "Usage:" on its own line
// followed by the command line, so the label alone never names the command.
func firstUsageLine(out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "Usage:") {
			continue
		}
		var b []string
		for j := i; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
			b = append(b, strings.TrimSpace(lines[j]))
		}
		return strings.Join(b, " ")
	}
	return ""
}

// checkHelp runs `args --help` through run, asserts it does not error, and
// asserts the printed page has a per-command Usage line naming token.
func checkHelp(t *testing.T, run func([]string) error, name string, args []string, token string) {
	t.Helper()
	out := captureStderr(t, func() {
		if err := run(append(args, "--help")); err != nil {
			t.Fatalf("%s --help returned error: %v", name, err)
		}
	})
	line := firstUsageLine(out)
	if line == "" {
		t.Errorf("%s --help printed no Usage line; got:\n%s", name, out)
		return
	}
	if !strings.Contains(line, token) {
		t.Errorf("%s --help Usage line %q does not name the command %q", name, line, token)
	}
}

// Every urnet-tools command whose -h renders a help page must show per-command
// usage.
func TestEveryToolsCommandHelpIsPerCommand(t *testing.T) {
	cmds := []struct{ name, token string }{
		{"providers", "providers"},
		{"status", "status"},
		{"start", "start"},
		{"stop", "stop"},
		{"restart", "restart"},
		{"update", "update"},
		{"idle-update", "idle-update"},
		{"self-update", "self-update"},
		{"logs", "logs"},
		{"summary", "summary"},
		{"default", "default"},
		{"session", "session"},
		{"turbo", "turbo"},
		{"auto", "auto"},
		{"eco", "eco"},
		{"lowmode", "lowmode"},
		{"ramlogs", "ramlogs"},
		{"optimize", "optimize"},
		{"hot-restart", "hot-restart"},
		{"fast-auth", "fast-auth"},
		{"set", "set"},
		{"auth", "auth"},
		{"choose-network", "choose-network"},
		{"proxy", "proxy"},
		{"report", "report"},
		{"hub", "hub"},
		{"reinstall", "reinstall"},
		{"uninstall", "uninstall"},
		{"auto-update", "auto-update"},
		{"auto-start", "auto-start"},
		{"self-heal", "self-heal"},
	}
	for _, c := range cmds {
		checkHelp(t, Run, c.name, []string{c.name}, c.token)
	}
}

// Everyday aliases resolve to the canonical command's per-command help page.
func TestToolsCommandAliasesHelpIsPerCommand(t *testing.T) {
	aliases := []struct{ alias, canonical string }{
		{"selfupdate", "self-update"},
		{"selfheal", "self-heal"},
		{"fastauth", "fast-auth"},
		{"autoupdate", "auto-update"},
		{"autostart", "auto-start"},
		{"hotrestart", "hot-restart"},
		{"choose_network", "choose-network"},
	}
	for _, a := range aliases {
		checkHelp(t, Run, a.alias, []string{a.alias}, a.canonical)
	}
}

// Every urnet-docker command whose -h renders a help page must show
// per-command usage, including the proxy parent and every proxy subcommand.
func TestEveryDockerCommandHelpIsPerCommand(t *testing.T) {
	run := RunDocker
	single := []struct{ name, token string }{
		{"providers", "providers"},
		{"status", "status"},
		{"start", "start"},
		{"stop", "stop"},
		{"restart", "restart"},
		{"logs", "logs"},
		{"auth", "auth"},
		{"choose-network", "choose-network"},
		{"summary", "summary"},
		{"report", "report"},
		{"update", "update"},
		{"idle-update", "idle-update"},
		{"self-heal", "self-heal"},
		{"set", "set"},
		{"fast-auth", "fast-auth"},
		{"hub", "hub"},
		{"session", "session"},
		{"exec", "exec"},
		{"direct", "direct"},
		{"usage", "usage"},
	}
	for _, c := range single {
		checkHelp(t, run, c.name, []string{c.name}, c.token)
	}
	// proxy parent renders its subcommand list, not the top-level menu.
	checkHelp(t, run, "proxy", []string{"proxy"}, "proxy")
	// every proxy subcommand renders its own help page.
	subs := []string{
		"add", "paste", "clear", "remove", "add-source", "remove-source",
		"refresh", "remove-dead", "health", "traffic", "summary", "trim", "exclude",
	}
	for _, sub := range subs {
		checkHelp(t, run, "proxy "+sub, []string{"proxy", sub}, sub)
	}
	// representative docker aliases
	for _, a := range []struct{ alias, canonical string }{
		{"selfheal", "self-heal"},
		{"fastauth", "fast-auth"},
	} {
		checkHelp(t, run, a.alias, []string{a.alias}, a.canonical)
	}
}

// TestVersionHelpIsRootIntercepted documents the intentional top-level
// intercept: -h on 'version' prints the tool version, not a help page. Both
// binaries must return nil and print the version without erroring.
func TestVersionHelpIsRootIntercepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func([]string) error
	}{
		{"urnet-tools", Run},
		{"urnet-docker", RunDocker},
	} {
		out := captureStdout(t, func() {
			if err := tc.run([]string{"version", "--help"}); err != nil {
				t.Fatalf("%s version --help returned error: %v", tc.name, err)
			}
		})
		if !strings.Contains(out, ToolVersion) {
			t.Errorf("%s version --help = %q, want it to contain %q", tc.name, out, ToolVersion)
		}
	}
}
