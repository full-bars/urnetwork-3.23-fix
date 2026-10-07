package urnettools

import (
	"os"
	"strings"
	"testing"
)

// renderBSDServiceScript produces the rc.d script the FreeBSD installer writes.
// The installer in scripts/Provider_Install_FreeBSD.sh carries its own copy of
// this template (a shell script cannot call into Go), so these assertions are
// about the Go-side renderer and the two must be kept in step by hand.
// The rc.d script the Go side emits is duplicated in the shell installer, and
// the two can drift. This test WRITES the rendered script to a file so the
// FreeBSD CI job can run it through `sh -n` on a real kernel.
//
// It writes a file rather than t.Log-ing the script: a go test log line is
// prefixed with "    file_test.go:NN: ", and that prefix is not valid shell —
// stripping it is guesswork, and getting it wrong would make the CI parse
// check assert against a mangled script.
func TestRenderBSDServiceScriptDumpsTemplate(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")
	if strings.TrimSpace(script) == "" {
		t.Fatal("renderBSDServiceScript returned an empty script")
	}
	// -test.run with a file path is the only way to get the exact bytes out
	// of a compiled test binary; the path comes from the caller in CI.
	out := os.Getenv("URN_RC_DUMP")
	if out == "" {
		t.Skip("URN_RC_DUMP not set; nothing to dump")
	}
	if err := os.WriteFile(out, []byte(script), 0o644); err != nil {
		t.Fatalf("write rendered rc.d: %v", err)
	}
	t.Logf("wrote %d bytes of rendered rc.d to %s", len(script), out)
}

func TestRenderBSDServiceScriptShape(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	// rc.subr provides run_rc_command; without it the script is not an rc.d
	// script at all.
	if !strings.Contains(script, ". /etc/rc.subr") {
		t.Error("script does not source rc.subr")
	}
	if !strings.Contains(script, "run_rc_command") {
		t.Error("script does not call run_rc_command")
	}
	// PROVIDE is what makes the script discoverable by the rc system.
	if !strings.Contains(script, "PROVIDE: urnetwork") {
		t.Error("script lacks the PROVIDE declaration")
	}
	// The knobs are read from rc.conf, which is what setAutoStart writes.
	for _, knob := range []string{"urnetwork_enable", "urnetwork_user", "urnetwork_flags"} {
		if !strings.Contains(script, knob) {
			t.Errorf("script does not read the %s rc.conf knob", knob)
		}
	}
	// The binary and working directory must be baked in, and the service must
	// run the provider's `provide` subcommand.
	// The provider is started through daemon(8), not run in the foreground:
	// a foreground `service <svc> start` would never return and would stall the
	// boot sequence, since rc runs start synchronously.
	if !strings.Contains(script, `daemon="/usr/sbin/daemon"`) {
		t.Error("script does not use daemon(8); a foreground provider would hang start and boot")
	}
	if !strings.Contains(script, "$daemon -f") {
		t.Error("script does not invoke daemon with -f")
	}
	// -p gives `service onestatus` a real pid to check; without it status always
	// reports "not running" and every start launches a duplicate.
	if !strings.Contains(script, `-p "$pidfile"`) {
		t.Error("script does not pass the child pidfile to daemon")
	}
	if !strings.Contains(script, `kill "$pid"`) {
		t.Error("script does not stop the provider through its pidfile")
	}
	// -u makes daemon set HOME/USER/SHELL for the target user, so the state dir
	// lands in that user's home rather than root's. It also replaces the old
	// `su -m`, which kept the caller's HOME.
	if !strings.Contains(script, `-u \${urnetwork_user}`) {
		t.Error("script does not run the provider as the rc.conf user via daemon -u")
	}
	// `su -m` appears in the explanatory comment, so assert on executable
	// lines only — a comment mentioning a mechanism is not that mechanism.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "su -m") {
			t.Errorf("script still executes su -m: %q", trimmed)
		}
	}
}

// The rc.conf variables are expanded at RUN time by the rc system, after
// load_rc_config has read /etc/rc.conf. A template that hard-codes the literal
// text "urnetwork_user" into the su(1) invocation therefore runs the provider
// as a user literally named "urnetwork_user", or fails outright — and it looks
// correct when read. This pins the expansion.
func TestRenderBSDServiceScriptExpandsUserVariable(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	// Every rc.conf knob must be referenced as a shell variable so it is expanded
	// at run time, after load_rc_config has read /etc/rc.conf. The ${...} form
	// is required where a default is wanted and $name where it is not.
	for _, want := range []string{
		"${urnetwork_enable:=",
		"${urnetwork_user:=",
		"${urnetwork_flags:=",
		"$urnetwork_flags",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not expand %q as a shell variable", want)
		}
	}
	// The bare (unexpanded) name must not appear as a command argument. The
	// match has to skip the "${...}" form, which is exactly the one we want.
	for _, bad := range []string{
		"su -m urnetwork_user",
		"su -m urnetwork_enable",
	} {
		if strings.Contains(script, bad) {
			t.Errorf("script passes a literal rc.conf knob name to su: %q", bad)
		}
	}
}

// A beta build and a stable build on one box must not collide on one service
// name, so the name comes from the binary.
func TestBSDRcServiceNameFromBinary(t *testing.T) {
	for _, c := range []struct{ binary, want string }{
		{"/usr/local/bin/urnetwork", "urnetwork"},
		{"/usr/local/bin/urnetwork_beta", "urnetwork_beta"},
		{"/usr/local/bin/urnet-provider", "urnet-provider"},
		{"/usr/local/bin/provider", "provider"},
		{"", "urnetwork"},
	} {
		if got := bsdRcServiceName(c.binary); got != c.want {
			t.Errorf("bsdRcServiceName(%q) = %q, want %q", c.binary, got, c.want)
		}
	}
}
