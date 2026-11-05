package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rc.d script's <service>_running_pid falls back to scanning the process
// table when the pidfile cannot identify the provider. That scan must see
// processes of EVERY user (daemon(8) publishes the pid before it changes user,
// and an unprivileged status call runs as someone else), yet still select only
// the provider: the binary and the "provide" subcommand are matched as argv
// fields, so neither an unrelated process nor the scan's own command line
// qualifies.
//
// Both copies of the script are exercised: the Go renderer and the shell
// installer's write_rc_script_body. The function is cut out of the rendered
// text and run under /bin/sh with a fake ps that serves a fixed table and
// honours -U the way ps(1) does, so restoring `ps -U <user>` changes the
// result. Nothing here runs on FreeBSD; it only needs a POSIX sh and awk.

const (
	pidScanService = "urnetwork"
	pidScanBinary  = "/usr/local/bin/urnetwork"
	pidScanUser    = "svc"
)

// pidScanTable is user<TAB>pid<TAB>args.
var pidScanTable = strings.Join([]string{
	"root\t100\t/usr/sbin/daemon -f -t urnetwork -u svc /usr/local/bin/urnetwork provide",
	"root\t200\t/usr/local/bin/urnetwork provide --port_ip=x",
	"other\t300\t/usr/local/bin/urnetwork-other provide",
	"other\t301\t/usr/local/bin/urnetwork status",
	"svc\t302\t/usr/bin/vi /usr/local/bin/urnetwork provide.txt",
	"svc\t400\tawk -v b=/usr/local/bin/urnetwork $2 == b && $3 == \"provide\" {print $1; exit}",
	"svc\t401\tsh -c ps -axo pid=,args= | grep /usr/local/bin/urnetwork provide",
}, "\n") + "\n"

const fakePS = `#!/bin/sh
user=""; all=0; p=""
while [ $# -gt 0 ]; do
	case "$1" in
	-U) user="$2"; shift ;;
	-p) p="$2"; shift ;;
	-axo|-ax) all=1 ;;
	esac
	shift
done
[ -z "$p" ] || exit 1
awk -F'\t' -v u="$user" -v all="$all" 'all == 1 || (u != "" && $1 == u) {printf "%5s %s\n", $2, $3}' "$MOCK_PS_TABLE"
`

// runningPidFunc cuts <service>_running_pid out of a rendered rc.d script.
func runningPidFunc(t *testing.T, rendered string) string {
	t.Helper()
	head := pidScanService + "_running_pid()\n{"
	i := strings.Index(rendered, head)
	if i < 0 {
		t.Fatalf("%s is missing from the rendered script", head)
	}
	rest := rendered[i:]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		t.Fatal("could not find the end of the running_pid function")
	}
	return rest[:j+3]
}

func runPidScan(t *testing.T, fn, table string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(fakePS), 0o755); err != nil {
		t.Fatal(err)
	}
	tbl := filepath.Join(dir, "table")
	if err := os.WriteFile(tbl, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	// pidfile points nowhere, so only the process-table scan can answer.
	harness := "pidfile=/nonexistent/none.pid\ndaemon=/usr/sbin/daemon\n" +
		pidScanService + "_user=" + pidScanUser + "\n" + fn + "\n" + pidScanService + "_running_pid\n"
	cmd := exec.Command("/bin/sh", "-c", harness)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MOCK_PS_TABLE="+tbl)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), code
}

func TestRunningPidScanSelectsOnlyProvider(t *testing.T) {
	goScript := renderBSDServiceScript(pidScanService, pidScanBinary, pidScanUser, "/var/db/urnetwork")
	shScript, err := renderInstallerRcdTemplate(t, pidScanService, pidScanBinary, pidScanUser)
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"go renderer": goScript, "shell installer": shScript} {
		t.Run(name, func(t *testing.T) {
			fn := runningPidFunc(t, script)

			// The provider runs as root, the service user is svc: a user
			// filter would miss it. The unrelated processes and the scan's
			// own command lines must not be picked.
			out, code := runPidScan(t, fn, pidScanTable)
			if code != 0 || out != "200" {
				t.Errorf("scan selected %q (exit %d), want only the cross-user provider 200", out, code)
			}

			// Without the provider the same table must yield nothing, so a
			// loosened match cannot pass by hitting a decoy.
			var noProvider []string
			for _, l := range strings.Split(strings.TrimSuffix(pidScanTable, "\n"), "\n") {
				if !strings.Contains(l, "\t200\t") {
					noProvider = append(noProvider, l)
				}
			}
			out, code = runPidScan(t, fn, strings.Join(noProvider, "\n")+"\n")
			if code == 0 || out != "" {
				t.Errorf("scan selected %q (exit %d) with no provider present, want none", out, code)
			}
		})
	}
}
