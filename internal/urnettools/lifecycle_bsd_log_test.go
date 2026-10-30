package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// daemon(8) opens the log path as root, so the rc.d start refuses a symlink
// outright and replaces anything else that is not a regular file with a single
// link owned by the service user. The two shapes a symlink check misses are
// the ones that matter: a fifo blocks that open for good, and a hard link makes
// the log write through to an aliased file.
//
// The start function is taken from the rc.d script the shipped installer
// renders (the shell template is executed, not retyped) and run under sh in a
// sandbox with a recording daemon. On FreeBSD, stat(1) -f takes the same
// format the guard uses, so the real binary answers. On Linux, stat(1) -f
// reports filesystem status instead, so the sandbox supplies a stat that
// answers the one format string the guard asks for; mkfifo, ln, open and
// unlink are real.
func TestFreeBSDStartReplacesUnsafeLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rc.d template is not a Windows artifact")
	}

	t.Run("fifo", func(t *testing.T) {
		s := newFreeBSDLogSandbox(t)
		if out, err := exec.Command("mkfifo", s.logPath).CombinedOutput(); err != nil {
			t.Skipf("mkfifo is not available: %v: %s", err, out)
		}
		if info, err := os.Lstat(s.logPath); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("the fixture is not a fifo (mode %v, err %v)", info.Mode(), err)
		}

		if rc, out := s.start(t, false); rc != 0 {
			t.Fatalf("start returned %d, want 0:\n%s", rc, out)
		}

		info, err := os.Lstat(s.logPath)
		if err != nil {
			t.Fatalf("the log path is gone after start: %v", err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("start kept a %s at the log path; the root daemon would block opening a fifo", info.Mode())
		}
		s.wantOneDaemonCall(t)
	})

	t.Run("hard link", func(t *testing.T) {
		s := newFreeBSDLogSandbox(t)
		aliased := filepath.Join(s.root, "aliased.log")
		if err := os.WriteFile(aliased, []byte("keep me\n"), 0o644); err != nil {
			t.Fatalf("write the aliased file: %v", err)
		}
		if err := os.Link(aliased, s.logPath); err != nil {
			t.Skipf("hard links are not available here: %v", err)
		}
		before, err := os.Stat(s.logPath)
		if err != nil {
			t.Fatalf("stat the hard link: %v", err)
		}

		if rc, out := s.start(t, false); rc != 0 {
			t.Fatalf("start returned %d, want 0:\n%s", rc, out)
		}

		after, err := os.Stat(s.logPath)
		if err != nil {
			t.Fatalf("the log path is gone after start: %v", err)
		}
		if os.SameFile(before, after) {
			t.Fatalf("start kept the hard link: the log path still names the inode behind %s, so provider output would be written through to it", aliased)
		}
		if got, err := os.ReadFile(aliased); err != nil || string(got) != "keep me\n" {
			t.Fatalf("the aliased file changed to %q (err %v); replacing the name must leave it alone", got, err)
		}
		s.wantOneDaemonCall(t)
	})

	t.Run("regular file", func(t *testing.T) {
		s := newFreeBSDLogSandbox(t)
		if err := os.WriteFile(s.logPath, []byte("previous run\n"), 0o600); err != nil {
			t.Fatalf("write the existing log: %v", err)
		}

		if rc, out := s.start(t, false); rc != 0 {
			t.Fatalf("start returned %d, want 0:\n%s", rc, out)
		}

		if got, err := os.ReadFile(s.logPath); err != nil || string(got) != "previous run\n" {
			t.Fatalf("start truncated the owned log to %q (err %v); every restart would erase the previous run", got, err)
		}
		s.wantOneDaemonCall(t)
	})

	t.Run("creation failure", func(t *testing.T) {
		s := newFreeBSDLogSandbox(t)

		rc, out := s.start(t, true)
		if rc == 0 {
			t.Fatalf("start succeeded with no log file and a failing install(1):\n%s", out)
		}
		if calls := s.daemonCalls(t); len(calls) != 0 {
			t.Fatalf("daemon(8) was started without a log file: %v", calls)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		s := newFreeBSDLogSandbox(t)
		target := filepath.Join(s.root, "target.log")
		if err := os.WriteFile(target, nil, 0o644); err != nil {
			t.Fatalf("write the symlink target: %v", err)
		}
		if err := os.Symlink(target, s.logPath); err != nil {
			t.Fatalf("plant the symlink: %v", err)
		}

		rc, out := s.start(t, false)
		if rc == 0 {
			t.Fatalf("start accepted a symlinked log path:\n%s", out)
		}
		if calls := s.daemonCalls(t); len(calls) != 0 {
			t.Fatalf("daemon(8) was started with a symlinked log path: %v", calls)
		}
	})
}

// freeBSDLogSandbox is a log dir, the rendered rc.d start function and the stub
// commands that function calls on a host where daemon(8) and FreeBSD stat(1)
// do not exist.
type freeBSDLogSandbox struct {
	root      string
	logPath   string
	stubDir   string
	daemonLog string
	startFn   string
	user      string
}

func newFreeBSDLogSandbox(t *testing.T) *freeBSDLogSandbox {
	t.Helper()
	me, err := user.Current()
	if err != nil || me.Username == "" {
		t.Skipf("cannot resolve the current user, so the owner half of the guard cannot be exercised: %v", err)
	}

	// Render through the shipped installer, not the Go template: this is the
	// copy a `curl | sh` install writes, and TestBSDTemplateParity keeps the
	// two identical line for line.
	script, err := renderInstallerRcdTemplate(t, "urnetwork", "/usr/local/bin/urnetwork", me.Username)
	if err != nil {
		t.Fatalf("render the rc.d script: %v", err)
	}
	startFn := extractRcdStart(t, script)
	if startFn == "" {
		t.Fatal("could not extract urnetwork_start from the rendered rc.d script")
	}

	s := &freeBSDLogSandbox{
		root:      t.TempDir(),
		logPath:   filepath.Join(t.TempDir(), "log", "stdout.log"),
		stubDir:   filepath.Join(t.TempDir(), "stub"),
		daemonLog: filepath.Join(t.TempDir(), "daemon.calls"),
		startFn:   startFn,
		user:      me.Username,
	}
	if strings.ContainsAny(s.root+s.stubDir+s.logPath+s.daemonLog, " '\"\t\n") {
		t.Skipf("sandbox paths need shell quoting this harness does not do: %s", s.root)
	}
	if err := os.MkdirAll(filepath.Dir(s.logPath), 0o755); err != nil {
		t.Fatalf("create the log dir: %v", err)
	}
	if err := os.MkdirAll(s.stubDir, 0o755); err != nil {
		t.Fatalf("create the stub dir: %v", err)
	}
	stubs := map[string]string{
		"daemon":  daemonStub,
		"install": installStub,
		"ps":      "#!/bin/sh\n# no provider is running in the sandbox\nexit 1\n",
	}
	// FreeBSD's stat(1) already answers -f FORMAT, so the guard runs against
	// the real binary there; the stand-in is only for the Linux host.
	if runtime.GOOS != "freebsd" {
		stubs["stat"] = statStub
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(s.stubDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}

	// A stat that mistranslates FreeBSD's %HT %l %Su would make the guard look
	// either too strict or too lax, so prove it answers for a file this user
	// owns before drawing any conclusion from the cases below.
	probe := filepath.Join(s.root, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("write the stat probe: %v", err)
	}
	statBin := "stat"
	if runtime.GOOS != "freebsd" {
		statBin = filepath.Join(s.stubDir, "stat")
	}
	out, err := exec.Command(statBin, "-f", "%HT %l %Su", probe).CombinedOutput()
	if want := "Regular File 1 " + me.Username; err != nil || strings.TrimSpace(string(out)) != want {
		// On FreeBSD the real stat(1) is the one the guard calls, so a mismatch
		// there is a broken environment; a skip would hide it.
		if runtime.GOOS == "freebsd" {
			t.Fatalf("stat -f cannot report %q on FreeBSD: %v: %q", want, err, out)
		}
		t.Skipf("the stat stub cannot report %q on this host: %v: %q", want, err, out)
	}
	return s
}

// start runs the rendered start function and returns its exit status. A failing
// install(1) is the only thing the caller can inject.
func (s *freeBSDLogSandbox) start(t *testing.T, failInstall bool) (int, string) {
	t.Helper()
	harness := filepath.Join(s.root, "harness.sh")
	body := fmt.Sprintf(`#!/bin/sh
daemon="daemon"
pidfile="%s/urnetwork.pid"
logdir="%s"
urnetwork_user="%s"
urnetwork_flags=""
name="urnetwork"
%s
urnetwork_start
rc=$?
echo "start-rc=$rc"
exit $rc
`, s.root, filepath.Dir(s.logPath), s.user, s.startFn)
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	cmd := exec.Command("/bin/sh", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+s.stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_DAEMON_LOG="+s.daemonLog,
	)
	if failInstall {
		cmd.Env = append(cmd.Env, "STUB_INSTALL_FAIL=1")
	}
	out, runErr := cmd.CombinedOutput()

	rc := -1
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "start-rc=") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(line, "start-rc="))
		if err != nil {
			t.Fatalf("unparsable start status %q: %v", line, err)
		}
		rc = n
	}
	if rc < 0 {
		t.Fatalf("the start function produced no status line (err %v):\n%s", runErr, out)
	}
	return rc, string(out)
}

func (s *freeBSDLogSandbox) daemonCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.daemonLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the recorded daemon calls: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

func (s *freeBSDLogSandbox) wantOneDaemonCall(t *testing.T) {
	t.Helper()
	calls := s.daemonCalls(t)
	if len(calls) != 1 {
		t.Fatalf("daemon(8) was invoked %d times, want once: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "-o "+s.logPath) {
		t.Errorf("daemon(8) was not pointed at the checked log path: %q", calls[0])
	}
}

// statStub answers FreeBSD's `stat -f '%HT %l %Su' <path>` from GNU stat, which
// spells the same three fields %F %h %U.
const statStub = `#!/bin/sh
if [ "$1" != "-f" ] || [ $# -lt 3 ]; then
    echo "stat stub: only -f FORMAT PATH is supported" >&2
    exit 1
fi
case $2 in
*%HT*%l*%Su*) ;;
*) echo "stat stub: unsupported format $2" >&2; exit 1 ;;
esac
path=$3
if [ -L "$path" ]; then kind="Symbolic Link"
elif [ -p "$path" ]; then kind="Fifo File"
elif [ -d "$path" ]; then kind="Directory"
elif [ -f "$path" ]; then kind="Regular File"
else echo "stat stub: $path has no type this stub knows" >&2; exit 1
fi
links=$(/usr/bin/stat -c %h "$path" 2>/dev/null) || exit 1
owner=$(/usr/bin/stat -c %U "$path" 2>/dev/null) || exit 1
printf '%s %s %s\n' "$kind" "$links" "$owner"
`

// installStub performs the one install(1) call the start function makes. It
// creates the file rather than copying /dev/null, and it can be made to fail so
// a swallowed creation error is visible.
const installStub = `#!/bin/sh
if [ -n "${STUB_INSTALL_FAIL:-}" ]; then
    echo "install stub: forced failure" >&2
    exit 1
fi
mode=""
while [ $# -gt 0 ]; do
    case $1 in
    -o) shift 2 ;;
    -m) mode=$2; shift 2 ;;
    *) break ;;
    esac
done
if [ $# -ne 2 ]; then
    echo "install stub: unsupported arguments" >&2
    exit 1
fi
: > "$2" || exit 1
[ -z "$mode" ] || chmod "$mode" "$2" || exit 1
`

// daemonStub records the call instead of opening the log, so a fifo at the log
// path cannot hang the test: that hang is the bug, not the assertion.
const daemonStub = `#!/bin/sh
printf '%s\n' "$*" >> "${STUB_DAEMON_LOG:?}"
`

// extractRcdStart returns the urnetwork_start definition from a rendered rc.d
// script. It cannot use installer_script_test.go's extractShellFunc: that looks
// for `name() {` on one line, and the rc.d template puts the brace on its own.
func extractRcdStart(t *testing.T, script string) string {
	t.Helper()
	const head = "urnetwork_start()\n{\n"
	i := strings.Index(script, head)
	if i < 0 {
		return ""
	}
	rest := script[i:]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		return ""
	}
	return rest[:j+len("\n}\n")]
}
