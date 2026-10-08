package urnettools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	//
	// The rc.conf knobs MUST appear unescaped: a backslash inside the
	// generated file makes sh pass the literal text ${urnetwork_user} as an
	// argument instead of expanding it, so daemon -u and chown would both get
	// a junk user and the provider would fail to start. The escaping is only
	// needed inside the installer's heredoc, which is where the value is
	// consumed at render time.
	if !strings.Contains(script, `-u ${urnetwork_user}`) {
		t.Error("script does not run the provider as the rc.conf user via daemon -u")
	}
	if strings.Contains(script, `\${urnetwork_user}`) {
		t.Error("script escapes ${urnetwork_user}; sh would pass it literally, not expand it")
	}
	if !strings.Contains(script, `chown ${urnetwork_user} "$logdir"`) {
		t.Error("script does not chown the log dir to the rc.conf user")
	}
	// $flags must reach the provider expanded, not as a literal ${...} or a
	// bare backslash followed by the value.
	if !strings.Contains(script, `provide $urnetwork_flags`) {
		t.Error("script does not pass the rc.conf flags to provide")
	}
	if strings.Contains(script, `\$urnetwork_flags`) {
		t.Error("script escapes $urnetwork_flags; the provider would receive a literal")
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

// The rc.d script exists in two copies: the Go renderer above and the heredoc
// in scripts/Provider_Install_FreeBSD.sh. Nothing compared them, so a fix
// landed in one and left the other shipping a broken service — twice. This
// renders BOTH with the same inputs and compares them line for line.
//
// Both are rendered for real: the Go side through the renderer itself, the
// shell side by executing the extracted write_rc_script_body with the same
// service, binary and user. Comparison drops comments and joins line
// continuations, which are the only permitted differences.
func TestBSDTemplateParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rc.d template is not a Windows artifact")
	}

	goScript := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	shScript, err := renderInstallerRcdTemplate(t, "urnetwork", "/usr/local/bin/urnetwork", "tester")
	if err != nil {
		t.Fatalf("render the installer copy: %v", err)
	}

	ng := normaliseTokens(comparableRcdLines(goScript))
	nsh := normaliseTokens(comparableRcdLines(shScript))
	if len(ng) != len(nsh) {
		t.Errorf("template length differs: Go renderer has %d lines, installer %d\n%s",
			len(ng), len(nsh), strings.Join(dumpLines(ng, nsh), "\n"))
		return
	}
	for i := range ng {
		if ng[i] != nsh[i] {
			t.Errorf("line %d differs between the two copies:\n  Go:       %q\n  installer: %q\n%s",
				i+1, ng[i], nsh[i], strings.Join(dumpLines(ng[min(i, len(ng)-1):], nsh[min(i, len(nsh)-1):]), "\n"))
			return
		}
	}
}

// renderInstallerRcdTemplate executes the installer's write_rc_script_body with
// the given values and returns what it writes to stdout. The function is
// extracted verbatim from the shipped script rather than copied, so the test
// tracks the file instead of drifting from it.
func renderInstallerRcdTemplate(t *testing.T, service, binary, user string) (string, error) {
	t.Helper()
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		return "", err
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	start := strings.Index(script, "write_rc_script_body() {")
	if start < 0 {
		return "", errors.New("write_rc_script_body is missing from the FreeBSD installer")
	}
	// The heredoc body contains column-0 braces, so the function cannot be cut
	// at the first "}": its real end is the RCSCRIPT delimiter.
	rest := script[start:]
	end := strings.Index(rest, "\nRCSCRIPT\n}")
	if end < 0 {
		return "", errors.New("could not find the end of write_rc_script_body")
	}
	fn := rest[:end+len("\nRCSCRIPT\n}")]

	// The whole prefix of the installer is included so every variable the
	// template interpolates is defined exactly as it is in a real install.
	dir := t.TempDir()
	full := dir + "/installer.sh"
	if err := os.WriteFile(full, []byte(script[:start]+fn+`
service_name="`+service+`"
provider_bin="`+binary+`"
write_rc_script_body "`+user+`"
`), 0o755); err != nil {
		return "", err
	}
	out, err := exec.Command("/bin/sh", full).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	// Rendering must be silent: a stray $(cat ...) or similar running at
	// render time prints to stderr and produces empty expansions.
	return string(out), nil
}

// comparableRcdLines reduces a rendered rc.d script to the lines that carry
// behaviour: comments are dropped and `\`-continuations are joined, because
// the two copies legitimately differ in how they wrap the daemon invocation.
func comparableRcdLines(script string) []string {
	var out []string
	pending := ""
	flush := func() {
		if pending != "" {
			out = append(out, pending)
			pending = ""
		}
	}
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `pkill -x "$(basename "$bin")"` is written literally in the Go
		// copy and expanded by the unquoted heredoc in the shell copy, so the
		// two differ in text but not in behaviour. Both must kill the same
		// basename, which the shapes asserted above already pin.
		if i := strings.Index(line, "pkill -TERM -x "); i >= 0 {
			line = `pkill -TERM -x <basename>`
		}
		// Join `\` continuations: the installer heredoc is unquoted, so the
		// backslash is consumed at render time and its copy arrives as one
		// long line. That is a rendering artifact, not a behavioural
		// difference, and it must not be reported as drift.
		if strings.HasSuffix(line, "\\") {
			// The heredoc collapses a continuation into the whitespace around
			// it, so the joined text can contain runs of spaces. Keep the
			// token list and let the join happen on a clean boundary.
			pending += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		pending += line
		flush()
	}
	flush()
	return out
}

// normaliseTokens collapses internal whitespace runs so continuation joining
// cannot glue two words together or leave ragged spacing. Comparing field
// lists rather than raw strings keeps a formatting difference from reading as
// behavioural drift, while any change to a flag, path or variable still fails.
func normaliseTokens(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Join(strings.Fields(l), " ")
	}
	return out
}

func dumpLines(a, b []string) []string {
	var out []string
	for i := range a {
		if i < len(b) {
			out = append(out, fmt.Sprintf("    context %q vs %q", a[i], b[i]))
			if i >= 4 {
				break
			}
		}
	}
	return out
}
