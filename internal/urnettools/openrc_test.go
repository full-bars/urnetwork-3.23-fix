//go:build linux

package urnettools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// openrcTestRig stubs the OpenRC backend's seams so routing tests exercise
// the real functions without touching /etc, rc-service, or systemd. active
// controls the detection outcome; installed controls whether the init script
// exists in a temp dir.
type openrcTestRig struct {
	argv [][]string
}

func (r *openrcTestRig) ran() int { return len(r.argv) }

func newOpenRCTestRig(t *testing.T, active, installed bool) *openrcTestRig {
	t.Helper()
	oldProbe, oldSystemd := openrcProbeFn, systemdRunningFn
	oldInit, oldRun, oldToolPath := openrcInitScriptPath, openrcRunFn, openrcToolPathFn
	oldPeriodic, oldExecutable := openrcPeriodicBaseDir, openrcExecutableFn
	oldDiscover := discoverSystemdFn
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() {
		openrcProbeFn, systemdRunningFn = oldProbe, oldSystemd
		openrcInitScriptPath, openrcRunFn, openrcToolPathFn = oldInit, oldRun, oldToolPath
		openrcPeriodicBaseDir, openrcExecutableFn = oldPeriodic, oldExecutable
		discoverSystemdFn = oldDiscover
		providerSupervisedByOpenRCFn = oldSupervised
	})
	// The restart gate enriches its audit line from discovery; stub it so a
	// test never touches (or writes a restart marker into) a real provider
	// state dir on the host running the tests.
	discoverSystemdFn = func() []Provider { return nil }
	// Default: a discovered provider counts as the service's supervised
	// process when it has a pid (the production check reads /proc/<pid>);
	// tests that need a bare provider override this.
	providerSupervisedByOpenRCFn = func(p Provider) bool { return p.PID > 0 }

	dir := t.TempDir()
	openrcInitScriptPath = filepath.Join(dir, "urnetwork")
	if installed {
		if err := os.WriteFile(openrcInitScriptPath, []byte("#!/sbin/openrc-run\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	openrcPeriodicBaseDir = filepath.Join(dir, "periodic")
	openrcProbeFn = func() bool { return active }
	systemdRunningFn = func() bool { return false }
	openrcToolPathFn = func(name string) string { return name }
	rig := &openrcTestRig{}
	openrcRunFn = func(args ...string) error {
		rig.argv = append(rig.argv, append([]string(nil), args...))
		return nil
	}
	return rig
}

// TestProbeOpenRCDetection pins the detection rule: rc-service must be
// resolvable AND /run/openrc must exist — the runtime marker OpenRC creates
// at boot. A host that merely has the openrc package installed (no boot) must
// NOT be treated as OpenRC: rc-service refuses every action there.
func TestProbeOpenRCDetection(t *testing.T) {
	oldLook, oldStat := openrcLookPathFn, openrcStatFn
	t.Cleanup(func() { openrcLookPathFn, openrcStatFn = oldLook, oldStat })

	cases := []struct {
		name      string
		rcService bool
		markers   map[string]bool
		want      bool
	}{
		{"rc-service + /run/openrc (booted)", true, map[string]bool{"/run/openrc": true}, true},
		{"rc-service + openrc-run only (packaged, never booted)", true, map[string]bool{"/sbin/openrc-run": true}, false},
		{"rc-service + /usr/sbin/openrc-run only", true, map[string]bool{"/usr/sbin/openrc-run": true}, false},
		{"rc-service, no markers", true, map[string]bool{}, false},
		{"no rc-service, /run/openrc present", false, map[string]bool{"/run/openrc": true}, false},
		{"nothing", false, map[string]bool{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			openrcLookPathFn = func(name string) (string, error) {
				if name == "rc-service" && c.rcService {
					return "/sbin/rc-service", nil
				}
				return "", os.ErrNotExist
			}
			openrcStatFn = func(path string) (os.FileInfo, error) {
				if c.markers[path] {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			if got := probeOpenRC(); got != c.want {
				t.Errorf("probeOpenRC() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOpenRCActiveSystemdWins pins the precedence rule: a host where systemd
// is RUNNING never routes through OpenRC, even if OpenRC is installed
// alongside — this is what keeps the systemd path unchanged.
func TestOpenRCActiveSystemdWins(t *testing.T) {
	oldProbe, oldSystemd := openrcProbeFn, systemdRunningFn
	t.Cleanup(func() { openrcProbeFn, systemdRunningFn = oldProbe, oldSystemd })

	openrcProbeFn = func() bool { return true }
	systemdRunningFn = func() bool { return true }
	if openrcActive() {
		t.Fatal("openrcActive() = true with systemd running; want false (systemd wins)")
	}
	systemdRunningFn = func() bool { return false }
	if !openrcActive() {
		t.Fatal("openrcActive() = false with OpenRC detected and no systemd; want true")
	}
	openrcProbeFn = func() bool { return false }
	if openrcActive() {
		t.Fatal("openrcActive() = true with no OpenRC detected; want false")
	}
}

// TestOpenRCRouteLifecycleArgv pins the command routing for start/stop/
// restart: exactly `rc-service urnetwork <verb>` with no systemctl anywhere.
func TestOpenRCRouteLifecycleArgv(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)

	for _, verb := range []string{"start", "stop", "restart"} {
		before := rig.ran()
		handled, err := openrcRouteLifecycle(verb, nil, true /*force: skip the restart prompt*/, false)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", verb, err)
		}
		if !handled {
			t.Fatalf("%s: handled = false, want true on an OpenRC host with the service installed", verb)
		}
		if rig.ran() != before+1 {
			t.Fatalf("%s: ran %d commands, want 1", verb, rig.ran()-before)
		}
		got := strings.Join(rig.argv[len(rig.argv)-1], " ")
		want := "rc-service urnetwork " + verb
		if got != want {
			t.Errorf("%s: argv = %q, want %q", verb, got, want)
		}
	}
}

// TestOpenRCRouteLifecycleFallthroughs pins the cases where the OpenRC
// backend must NOT act and the caller's normal path runs instead.
func TestOpenRCRouteLifecycleFallthroughs(t *testing.T) {
	t.Run("service not installed", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, false)
		handled, err := openrcRouteLifecycle("start", nil, false, false)
		if handled || err != nil {
			t.Fatalf("handled=%v err=%v, want false,nil when the service is not installed", handled, err)
		}
		if rig.ran() != 0 {
			t.Fatal("no command may run when the service is not installed")
		}
	})
	t.Run("not an OpenRC host", func(t *testing.T) {
		rig := newOpenRCTestRig(t, false, true)
		handled, _ := openrcRouteLifecycle("stop", nil, false, false)
		if handled {
			t.Fatal("handled = true on a non-OpenRC host")
		}
		if rig.ran() != 0 {
			t.Fatal("no command may run on a non-OpenRC host")
		}
	})
	t.Run("explicit foreign unit", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		handled, _ := openrcRouteLifecycle("restart", []string{"--unit", "other.service"}, true, false)
		if handled {
			t.Fatal("handled = true for an explicit foreign unit; the normal systemd path must run")
		}
		if rig.ran() != 0 {
			t.Fatal("no command may run for a foreign unit")
		}
	})
	t.Run("leftover positional", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		handled, _ := openrcRouteLifecycle("stop", []string{"ps"}, false, false)
		if handled {
			t.Fatal("handled = true with a leftover positional; the normal path must refuse it")
		}
		if rig.ran() != 0 {
			t.Fatal("no command may run with a leftover positional")
		}
	})
	t.Run("explicit service unit accepted", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		handled, err := openrcRouteLifecycle("start", []string{"--unit", "urnetwork"}, false, false)
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v, want true,nil for --unit urnetwork", handled, err)
		}
		if rig.ran() != 1 {
			t.Fatal("expected exactly one rc-service invocation")
		}
	})
	t.Run("dry-run plans without acting", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		handled, err := openrcRouteLifecycle("restart", nil, false, true)
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v, want true,nil", handled, err)
		}
		if rig.ran() != 0 {
			t.Fatal("dry-run must not execute rc-service")
		}
	})
}

// TestOpenRCAutoStartArgv pins auto-start to rc-update add|del with the
// EXPLICIT default runlevel (a bare `rc-update del urnetwork` targets
// sysinit and fails).
func TestOpenRCAutoStartArgv(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)

	handled, err := openrcRouteAutoStart("on", nil, false, false)
	if !handled || err != nil {
		t.Fatalf("auto-start on: handled=%v err=%v", handled, err)
	}
	handled, err = openrcRouteAutoStart("off", nil, false, false)
	if !handled || err != nil {
		t.Fatalf("auto-start off: handled=%v err=%v", handled, err)
	}
	if rig.ran() != 2 {
		t.Fatalf("ran %d commands, want 2", rig.ran())
	}
	if got, want := strings.Join(rig.argv[0], " "), "rc-update add urnetwork default"; got != want {
		t.Errorf("on argv = %q, want %q", got, want)
	}
	if got, want := strings.Join(rig.argv[1], " "), "rc-update del urnetwork default"; got != want {
		t.Errorf("off argv = %q, want %q", got, want)
	}

	// Not installed: no rc-update for a service that does not exist.
	rig2 := newOpenRCTestRig(t, true, false)
	handled, err = openrcRouteAutoStart("on", nil, false, false)
	if handled || err != nil || rig2.ran() != 0 {
		t.Fatalf("auto-start on without the service installed: handled=%v err=%v ran=%d, want false,nil,0", handled, err, rig2.ran())
	}
}

// TestOpenRCAutoUpdatePeriodicEntry pins the busybox crond periodic-entry
// mechanism: the entry lands in /etc/periodic/<interval>/urnetwork-update
// (temp dir in tests), is executable, runs `update -f`, moves cleanly
// between intervals, and is removed by `off`.
func TestOpenRCAutoUpdatePeriodicEntry(t *testing.T) {
	newOpenRCTestRig(t, true, true)

	if err := openrcSetAutoUpdate("weekly", "/opt/urnet/urnet-tools"); err != nil {
		t.Fatalf("set weekly: %v", err)
	}
	weekly := filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)
	fi, err := os.Stat(weekly)
	if err != nil {
		t.Fatalf("weekly entry missing: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("weekly entry mode = %v, want executable (run-parts skips non-executables)", fi.Mode())
	}
	b, err := os.ReadFile(weekly)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	// The entry names the tool in a variable and execs the variable, because a
	// guard verifies the path is not reachable through a user-writable
	// directory before exec'ing (the entry runs as root).
	if !strings.Contains(content, `tool='/opt/urnet/urnet-tools'`) {
		t.Fatalf("entry content = %q, want it to set tool='/opt/urnet/urnet-tools'", content)
	}
	if !strings.Contains(content, `exec "$tool" update -f`) {
		t.Fatalf("entry content = %q, want an exec of the tool with `update -f`", content)
	}
	// The guard is the security property: root must not execute a binary an
	// unprivileged user could have planted by renaming an ancestor directory.
	if !strings.Contains(content, "writable by a non-root user") {
		t.Fatalf("entry content = %q, want the root-ownership guard", content)
	}
	if !strings.HasPrefix(content, "#!/bin/sh\n") {
		t.Fatalf("entry must start with a #!/bin/sh shebang, got %q", content)
	}

	// Interval change moves the entry; it must not leave two queues firing.
	if err := openrcSetAutoUpdate("daily", "/opt/urnet/urnet-tools"); err != nil {
		t.Fatalf("set daily: %v", err)
	}
	if _, err := os.Stat(weekly); !os.IsNotExist(err) {
		t.Fatalf("weekly entry still present after switching to daily (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "daily", openrcUpdateCronName)); err != nil {
		t.Fatalf("daily entry missing: %v", err)
	}

	// off removes every queue's entry.
	if err := openrcSetAutoUpdate("off", ""); err != nil {
		t.Fatalf("off: %v", err)
	}
	for _, dir := range openrcPeriodicIntervals {
		if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronName)); !os.IsNotExist(err) {
			t.Fatalf("%s entry still present after off (err=%v)", dir, err)
		}
	}
	// off on a never-enabled service is a no-op, not an error.
	if err := openrcSetAutoUpdate("off", ""); err != nil {
		t.Fatalf("second off: %v", err)
	}
	// Invalid intervals are refused.
	if err := openrcSetAutoUpdate("hourly", "/x"); err == nil {
		t.Fatal("hourly must be refused (not a periodic queue)")
	}
}

// TestOpenRCAutoUpdateRouting exercises the cmdAutoUpdate route end to end:
// on an OpenRC host it writes the periodic entry instead of a timer.
func TestOpenRCAutoUpdateRouting(t *testing.T) {
	newOpenRCTestRig(t, true, true)
	if err := cmdAutoUpdate([]string{"weekly"}, false, false); err != nil {
		t.Fatalf("cmdAutoUpdate(weekly) on OpenRC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); err != nil {
		t.Fatalf("cmdAutoUpdate(weekly) did not write the periodic entry: %v", err)
	}
	if err := cmdAutoUpdate([]string{"off"}, false, false); err != nil {
		t.Fatalf("cmdAutoUpdate(off) on OpenRC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); !os.IsNotExist(err) {
		t.Fatalf("cmdAutoUpdate(off) left the periodic entry behind (err=%v)", err)
	}
	// Dry run: plan only, nothing written.
	if err := cmdAutoUpdate([]string{"monthly"}, false, true); err != nil {
		t.Fatalf("cmdAutoUpdate(monthly, dry-run): %v", err)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "monthly", openrcUpdateCronName)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote the periodic entry (err=%v)", err)
	}
}

// TestOpenRCRestartServiceRouting pins the update-flow restart: only a
// provider process that supervise-daemon supervises is restarted through
// rc-service; anything else falls through to the existing path.
func TestOpenRCRestartServiceRouting(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSupervised })

	rig := newOpenRCTestRig(t, true, true)
	providerSupervisedByOpenRCFn = func(p Provider) bool { return true }

	handled, err := openrcRestartService(Provider{PID: 4242, StateDir: "/home/urnet/.urnetwork"})
	if !handled || err != nil {
		t.Fatalf("supervised provider: handled=%v err=%v, want true,nil", handled, err)
	}
	if rig.ran() != 1 || strings.Join(rig.argv[0], " ") != "rc-service urnetwork restart" {
		t.Fatalf("argv = %v, want [rc-service urnetwork restart]", rig.argv)
	}

	// A bare (unsupervised) process is not the service's: no rc-service.
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
	handled, err = openrcRestartService(Provider{PID: 4242})
	if handled || err != nil || rig.ran() != 1 {
		t.Fatalf("unsupervised provider: handled=%v err=%v ran=%d, want false,nil,1", handled, err, rig.ran())
	}

	// A systemd-unit provider is never hijacked, even when supervised.
	providerSupervisedByOpenRCFn = func(p Provider) bool { return true }
	handled, _ = openrcRestartService(Provider{Unit: "urnetwork.service", PID: 4242})
	if handled {
		t.Fatal("systemd-unit provider must not route through OpenRC")
	}
}

// TestOpenRCHotSwapDecline pins the no-hotswap trade: a supervise-daemon
// provider declines HotSwap cleanly (falling back to a stop/start restart),
// while other unitless providers (docker PID 1, bare processes) keep their
// existing behavior.
func TestOpenRCHotSwapDecline(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSupervised })

	providerSupervisedByOpenRCFn = func(p Provider) bool { return true }
	if err := hotSwapUnitOK(Provider{PID: 4242}); err != ErrHotSwapOpenRC {
		t.Fatalf("hotSwapUnitOK(supervised) = %v, want ErrHotSwapOpenRC", err)
	}
	if got := hotswapDeclineReason(ErrHotSwapOpenRC); got != "openrc" {
		t.Fatalf("hotswapDeclineReason(ErrHotSwapOpenRC) = %q, want %q", got, "openrc")
	}
	// The full preflight must surface the same clean decline, not an error
	// about versions or unit types.
	if err := hotSwapPreflight(Provider{PID: 4242, Version: "v3.23.0-fix.31.0"}); err != ErrHotSwapOpenRC {
		t.Fatalf("hotSwapPreflight(supervised) = %v, want ErrHotSwapOpenRC", err)
	}

	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
	if err := hotSwapUnitOK(Provider{PID: 4242}); err != nil {
		t.Fatalf("hotSwapUnitOK(unsupervised unitless) = %v, want nil (unchanged behavior)", err)
	}
}

// TestIsSuperviseDaemonComm pins the /proc comm match, including the
// kernel's 15-byte truncation of "supervise-daemon" and the rejection of
// short comms that a prefix test would wrongly accept ("su" is a prefix of
// "supervise-daemon" — every su-parented provider would be misclassified).
func TestIsSuperviseDaemonComm(t *testing.T) {
	for _, c := range []struct {
		comm string
		want bool
	}{
		{"supervise-daemon", true},
		{"supervise-daemo", true}, // TASK_COMM_LEN truncation
		{"", false},
		{"su", false},
		{"s", false},
		{"sup", false},
		{"supervise", false},
		{"go", false},
		{"supervised", false},
		{"init", false},
	} {
		if got := isSuperviseDaemonComm(c.comm); got != c.want {
			t.Errorf("isSuperviseDaemonComm(%q) = %v, want %v", c.comm, got, c.want)
		}
	}
}

// TestParentPID reads the real /proc for this test process: its parent must
// match os.Getppid(). The pure parser is pinned separately (TestParseParentPID).
func TestParentPID(t *testing.T) {
	if got, want := parentPID(os.Getpid()), os.Getppid(); got != want {
		t.Fatalf("parentPID(self) = %d, want %d", got, want)
	}
	if got := parentPID(1); got != 0 {
		t.Fatalf("parentPID(1) = %d, want 0 (init has no parent)", got)
	}
}

// TestParseParentPID pins the /proc/<pid>/stat parser, including a comm that
// contains spaces and parentheses (the reason only fields after the LAST ')'
// are parsed).
func TestParseParentPID(t *testing.T) {
	for _, c := range []struct {
		stat string
		want int
	}{
		{"1234 (supervise-daemo) S 1 1234 1234 0 -1 4194560", 1},
		{"1234 (bash) S 5678 1234 1234 0 -1 4194560", 5678},
		{"1234 (a b) c) R 42 1234 1234", 42}, // comm with spaces and a ')'
		{"1234 (x) S", 0},                    // truncated: no ppid field
		{"garbage", 0},
		{"", 0},
	} {
		if got := parseParentPID(c.stat); got != c.want {
			t.Errorf("parseParentPID(%q) = %d, want %d", c.stat, got, c.want)
		}
	}
}

// TestOpenRCCleanupRemovesArtifacts pins uninstall cleanup: the init script
// and periodic entries are removed and the service is de-registered.
func TestOpenRCCleanupRemovesArtifacts(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	// Simulate an enabled auto-update entry.
	if err := openrcSetAutoUpdate("weekly", "/x/urnet-tools"); err != nil {
		t.Fatal(err)
	}

	openrcCleanup()

	if rig.ran() != 2 {
		t.Fatalf("cleanup ran %d commands, want 2 (stop + del)", rig.ran())
	}
	if got := strings.Join(rig.argv[0], " "); got != "rc-service urnetwork stop" {
		t.Errorf("first command = %q, want the service stop", got)
	}
	if got := strings.Join(rig.argv[1], " "); got != "rc-update del urnetwork default" {
		t.Errorf("second command = %q, want the rc-update del", got)
	}
	if _, err := os.Stat(openrcInitScriptPath); !os.IsNotExist(err) {
		t.Fatalf("init script still present after cleanup (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); !os.IsNotExist(err) {
		t.Fatalf("periodic entry still present after cleanup (err=%v)", err)
	}
}

// TestOpenRCToolPathResolution pins the auto-update tool resolution order:
// the tool beside the provider binary wins, then the running executable.
func TestOpenRCToolPathResolution(t *testing.T) {
	oldStat, oldExec, oldLook := openrcStatFn, openrcExecutableFn, openrcLookPathFn
	t.Cleanup(func() { openrcStatFn, openrcExecutableFn, openrcLookPathFn = oldStat, oldExec, oldLook })

	dir := t.TempDir()
	providerBin := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(providerBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No sibling tool, no PATH tool: falls to the running executable.
	openrcLookPathFn = func(string) (string, error) { return "", os.ErrNotExist }
	openrcExecutableFn = func() (string, error) { return "/usr/local/bin/urnet-tools", nil }
	openrcStatFn = func(path string) (os.FileInfo, error) { return os.Stat(path) }
	if got := openrcUpdateToolPath(Provider{Binary: providerBin}); got != "/usr/local/bin/urnet-tools" {
		t.Fatalf("tool path = %q, want the running executable fallback", got)
	}
	// A PATH tool wins over the running executable: a transient copy (a fresh
	// build in /tmp) must not be baked into the persistent cron entry.
	openrcLookPathFn = func(name string) (string, error) {
		if name == "urnet-tools" {
			return "/usr/local/bin/urnet-tools", nil
		}
		return "", os.ErrNotExist
	}
	openrcExecutableFn = func() (string, error) { return "/tmp/transient/urnet-tools", nil }
	if got := openrcUpdateToolPath(Provider{Binary: providerBin}); got != "/usr/local/bin/urnet-tools" {
		t.Fatalf("tool path = %q, want the PATH tool ahead of the transient executable", got)
	}
	// Sibling tool present: it wins over everything.
	openrcLookPathFn = func(string) (string, error) { return "", os.ErrNotExist }
	sibling := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := openrcUpdateToolPath(Provider{Binary: providerBin}); got != sibling {
		t.Fatalf("tool path = %q, want the sibling %q", got, sibling)
	}
}

// TestOpenRCRouteSelectorFlags pins the selector-flag contract: a selector
// that matches the service's own discovered provider routes to the service;
// one that points somewhere else (a bare process's pid, another state dir)
// must NOT be silently retargeted onto the service.
func TestOpenRCRouteSelectorFlags(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "urnet", StateDir: "/home/urnet/.urnetwork", Network: "net1", PID: 4242}}
	}

	// Matching selectors route to the service.
	for _, args := range [][]string{
		{"--state-dir", "/home/urnet/.urnetwork"},
		{"--user", "urnet"},
		{"--network", "net1"},
		{"--pid", "4242"},
		{"--unit", "urnetwork"},
	} {
		before := rig.ran()
		handled, err := openrcRouteLifecycle("stop", args, false, false)
		if !handled || err != nil {
			t.Fatalf("matching selector %v: handled=%v err=%v, want true,nil", args, handled, err)
		}
		if rig.ran() != before+1 {
			t.Fatalf("matching selector %v: no rc-service ran", args)
		}
		if got := strings.Join(rig.argv[len(rig.argv)-1], " "); got != "rc-service urnetwork stop" {
			t.Fatalf("matching selector %v: argv = %q", args, got)
		}
	}

	// Foreign selectors fall through to the normal path (which errors with
	// its own precise message) instead of acting on the service.
	for _, args := range [][]string{
		{"--pid", "9999"},
		{"--state-dir", "/home/other/.urnetwork"},
		{"--user", "other"},
		{"--network", "othernet"},
	} {
		before := rig.ran()
		handled, err := openrcRouteLifecycle("stop", args, false, false)
		if handled || err != nil {
			t.Fatalf("foreign selector %v: handled=%v err=%v, want false,nil", args, handled, err)
		}
		if rig.ran() != before {
			t.Fatalf("foreign selector %v: rc-service must not run", args)
		}
	}

	// Ambiguous discovery for the SELECTOR (two providers share the selector
	// value): no guessing, fall through.
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "alice", PID: 1, StateDir: "/a"}, {User: "alice", PID: 2, StateDir: "/b"}}
	}
	handled, _ := openrcRouteLifecycle("stop", []string{"--user", "alice"}, false, false)
	if handled {
		t.Fatal("a selector matching multiple providers must fall through")
	}

	// A --pid match is never ambiguous just because OTHER providers exist:
	// it names one process, and routing applies when that process is the
	// service's supervised child.
	discoverSystemdFn = func() []Provider {
		return []Provider{
			{User: "urnet", StateDir: "/home/urnet/.urnetwork", PID: 4242}, // supervised (rig default)
			{User: "alice", StateDir: "/home/alice/.urnetwork", PID: 777},  // bare
		}
	}
	providerSupervisedByOpenRCFn = func(p Provider) bool { return p.PID == 4242 }
	before := rig.ran()
	handled, err := openrcRouteLifecycle("stop", []string{"--pid", "4242"}, false, false)
	if !handled || err != nil {
		t.Fatalf("--pid of the supervised provider among several: handled=%v err=%v, want true,nil", handled, err)
	}
	if rig.ran() != before+1 {
		t.Fatal("rc-service did not run for the supervised --pid match")
	}
	// The same selector naming the BARE provider must not act on the service.
	handled, _ = openrcRouteLifecycle("stop", []string{"--pid", "777"}, false, false)
	if handled {
		t.Fatal("--pid of a bare provider must fall through")
	}
}

// TestOpenRCAutoStartRouting exercises cmdAutoStart end to end on OpenRC and
// pins the leftover-positional refusal (a mistyped command must not silently
// run rc-update).
func TestOpenRCAutoStartRouting(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)

	if err := cmdAutoStart([]string{"on"}, false, false); err != nil {
		t.Fatalf("cmdAutoStart(on): %v", err)
	}
	if err := cmdAutoStart([]string{"off"}, false, false); err != nil {
		t.Fatalf("cmdAutoStart(off): %v", err)
	}
	if rig.ran() != 2 {
		t.Fatalf("ran %d commands, want 2", rig.ran())
	}
	if got, want := strings.Join(rig.argv[0], " "), "rc-update add urnetwork default"; got != want {
		t.Errorf("on argv = %q, want %q", got, want)
	}
	if got, want := strings.Join(rig.argv[1], " "), "rc-update del urnetwork default"; got != want {
		t.Errorf("off argv = %q, want %q", got, want)
	}

	before := rig.ran()
	if err := cmdAutoStart([]string{"on", "typo"}, false, false); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("cmdAutoStart(on typo) = %v, want a positional-arguments error", err)
	}
	if rig.ran() != before {
		t.Fatal("a mistyped auto-start must not run rc-update")
	}
}

// TestOpenRCAutoUpdateEntryUsesSiblingTool: the periodic entry must exec the
// urnet-tools beside the provider binary (the canonical install layout), not
// whichever transient binary happens to be running.
func TestOpenRCAutoUpdateEntryUsesSiblingTool(t *testing.T) {
	newOpenRCTestRig(t, true, true)

	dir := t.TempDir()
	providerBin := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(providerBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "urnet", StateDir: "/home/urnet/.urnetwork", PID: 4242, Binary: providerBin}}
	}

	if err := cmdAutoUpdate([]string{"weekly"}, false, false); err != nil {
		t.Fatalf("cmdAutoUpdate(weekly): %v", err)
	}
	b, err := os.ReadFile(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `tool='`+sibling+`'`) {
		t.Fatalf("entry = %q, want it to name the sibling tool %q", b, sibling)
	}
	if !strings.Contains(string(b), `exec "$tool" update -f`) {
		t.Fatalf("entry = %q, want an exec of the named tool", b)
	}

	// Leftover positionals are refused here too.
	if err := cmdAutoUpdate([]string{"weekly", "typo"}, false, false); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("cmdAutoUpdate(weekly typo) = %v, want a positional-arguments error", err)
	}
}

// TestOpenRCBareProviderNotTreatedAsService: a running BARE provider (manual
// launch, service stopped) is discoverable on an OpenRC host but is NOT the
// service's process. Selectors must not match it, and the restart gate must
// not write a restart marker into its state dir.
func TestOpenRCBareProviderNotTreatedAsService(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false } // a bare provider
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "alice", StateDir: "/home/alice/.urnetwork", PID: 777}}
	}

	// A selector matching the bare provider must NOT route to the service.
	handled, err := openrcRouteLifecycle("stop", []string{"--pid", "777"}, false, false)
	if handled || err != nil {
		t.Fatalf("bare-provider selector: handled=%v err=%v, want false,nil (falls through)", handled, err)
	}
	if rig.ran() != 0 {
		t.Fatal("rc-service must not run for a bare provider")
	}

	// The gate provider must not be enriched from the bare provider: no
	// restart marker for an unrelated state dir.
	gp := openrcGateProvider(Target{})
	if gp.StateDir != "" || gp.User != "" || gp.PID != 0 {
		t.Fatalf("gate provider enriched from a bare provider: %+v", gp)
	}
}

// TestOpenRCAutoRoutesSelectorFallthrough: the auto-start/auto-update routes
// apply the same selector contract as the lifecycle route.
func TestOpenRCAutoRoutesSelectorFallthrough(t *testing.T) {
	newOpenRCTestRig(t, true, true)
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "urnet", StateDir: "/home/urnet/.urnetwork", PID: 4242}}
	}

	if handled, _ := openrcRouteAutoStart("on", []string{"--user", "other"}, false, false); handled {
		t.Fatal("auto-start with a foreign --user must fall through")
	}
	if handled, _ := openrcRouteAutoUpdate("weekly", []string{"--user", "other"}, false); handled {
		t.Fatal("auto-update with a foreign --user must fall through")
	}
	if handled, err := openrcRouteAutoStart("on", []string{"--user", "urnet"}, false, false); !handled || err != nil {
		t.Fatalf("auto-start with a matching --user: handled=%v err=%v, want true,nil", handled, err)
	}
}

// TestOpenRCCrondNote pins the three crond-note branches: not installed,
// stopped (exit 3), probe failure — and the quiet happy path.
func TestOpenRCCrondNote(t *testing.T) {
	oldStat, oldProbe := openrcStatFn, openrcCrondStatusFn
	t.Cleanup(func() { openrcStatFn, openrcCrondStatusFn = oldStat, oldProbe })

	// Not installed: no /etc/init.d/crond.
	openrcStatFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	if note := openrcCrondNote(); !strings.Contains(note, "not installed") {
		t.Fatalf("not-installed note = %q", note)
	}

	// Installed: the probe decides.
	openrcStatFn = func(path string) (os.FileInfo, error) {
		if path == "/etc/init.d/crond" {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	openrcCrondStatusFn = func() error { return nil }
	if note := openrcCrondNote(); note != "" {
		t.Fatalf("started crond note = %q, want empty", note)
	}
	stoppedErr := exec.Command("sh", "-c", "exit 3").Run() // real *exec.ExitError, code 3
	openrcCrondStatusFn = func() error { return stoppedErr }
	if note := openrcCrondNote(); !strings.Contains(note, "not started") {
		t.Fatalf("stopped crond note = %q", note)
	}
	openrcCrondStatusFn = func() error { return errors.New("boom") }
	if note := openrcCrondNote(); !strings.Contains(note, "could not check") {
		t.Fatalf("probe-failure note = %q", note)
	}
}

// TestOpenRCCleanupTargetAware: cleanupLifecycle is target-aware — the
// service artifacts are removed only when the uninstall targets the service's
// own supervised provider; a bare provider (or a systemd-unit provider) must
// not tear the service down; and the periodic entry is still swept when no
// service exists.
func TestOpenRCCleanupTargetAware(t *testing.T) {
	oldTimer := systemdTimerDisableFn
	var timerArgs [][]string
	systemdTimerDisableFn = func(args ...string) error {
		timerArgs = append(timerArgs, append([]string(nil), args...))
		return nil
	}
	t.Cleanup(func() { systemdTimerDisableFn = oldTimer })

	t.Run("supervised provider: full openrc cleanup, no systemd timer", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		timerArgs = nil
		cleanupLifecycle(Provider{PID: 4242})
		if rig.ran() != 2 {
			t.Fatalf("openrc cleanup ran %d commands, want 2", rig.ran())
		}
		if len(timerArgs) != 0 {
			t.Fatalf("systemd timer disable ran for a unitless provider: %v", timerArgs)
		}
	})

	t.Run("stopped OpenRC provider: full cleanup", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		cleanupLifecycle(Provider{Supervisor: "openrc"})
		if rig.ran() != 2 {
			t.Fatalf("stopped service cleanup ran %d commands, want 2", rig.ran())
		}
		if _, err := os.Stat(openrcInitScriptPath); !os.IsNotExist(err) {
			t.Fatalf("service script remains: %v", err)
		}
	})

	t.Run("bare provider with the service installed: leave the service alone", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
		timerArgs = nil
		if err := openrcSetAutoUpdate("weekly", "/x/urnet-tools"); err != nil {
			t.Fatal(err)
		}
		cleanupLifecycle(Provider{PID: 777})
		if rig.ran() != 0 {
			t.Fatalf("rc-service must not run for a bare provider; ran %v", rig.argv)
		}
		if _, err := os.Stat(openrcInitScriptPath); err != nil {
			t.Fatalf("init script removed for a bare-provider uninstall: %v", err)
		}
		if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); err != nil {
			t.Fatalf("periodic entry removed for a bare-provider uninstall: %v", err)
		}
	})

	t.Run("systemd-unit provider: openrc service untouched, timer disabled", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, true)
		timerArgs = nil
		cleanupLifecycle(Provider{Unit: "urnetwork-mig.service"})
		if rig.ran() != 0 {
			t.Fatalf("openrc cleanup ran for a systemd-unit provider: %v", rig.argv)
		}
		if len(timerArgs) != 1 {
			t.Fatalf("systemd timer disable ran %d times, want 1", len(timerArgs))
		}
		if got := strings.Join(timerArgs[0], " "); !strings.Contains(got, "-update.timer") {
			t.Fatalf("timer disable args = %q, want the -update.timer unit", got)
		}
	})

	t.Run("no service installed: sweep the cron entry only", func(t *testing.T) {
		rig := newOpenRCTestRig(t, true, false)
		if err := openrcSetAutoUpdate("weekly", "/x/urnet-tools"); err != nil {
			t.Fatal(err)
		}
		cleanupLifecycle(Provider{})
		if rig.ran() != 0 {
			t.Fatalf("rc-service must not run without an installed service; ran %v", rig.argv)
		}
		if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); !os.IsNotExist(err) {
			t.Fatalf("periodic entry still present after cleanup (err=%v)", err)
		}
	})
}

// TestOpenRCSudoHintOnPermissionError: a non-root run cannot manage the
// /etc/periodic queue; the error must say how to elevate.
func TestOpenRCSudoHintOnPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based test cannot run as root")
	}
	newOpenRCTestRig(t, true, true)
	base := t.TempDir()
	ro := filepath.Join(base, "periodic")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	openrcPeriodicBaseDir = ro
	err := openrcSetAutoUpdate("weekly", "/x/urnet-tools")
	if err == nil {
		t.Fatal("expected a permission error writing into a read-only queue")
	}
	if !strings.Contains(err.Error(), "re-run as root") {
		t.Fatalf("error = %v, want the sudo hint", err)
	}
}

// TestOpenRCCleanupSweepsCronWithoutService: the periodic auto-update entry
// can exist without the init script (auto-update does not require the
// service); uninstall must still remove it — and must not run rc-service
// against a service that is not installed.
func TestOpenRCCleanupSweepsCronWithoutService(t *testing.T) {
	rig := newOpenRCTestRig(t, true, false) // active, service NOT installed
	if err := openrcSetAutoUpdate("weekly", "/x/urnet-tools"); err != nil {
		t.Fatal(err)
	}

	cleanupLifecycle(Provider{})

	if rig.ran() != 0 {
		t.Fatalf("rc-service must not run without an installed service; ran %v", rig.argv)
	}
	if _, err := os.Stat(filepath.Join(openrcPeriodicBaseDir, "weekly", openrcUpdateCronName)); !os.IsNotExist(err) {
		t.Fatalf("periodic entry still present after cleanup (err=%v)", err)
	}
}

// TestOpenRCLifecycleFallsThroughToBareProvider: with the service stopped and
// only a bare provider running, start/stop/restart must NOT act on the
// service (the normal unitless path handles the running provider — rc-service
// stop would falsely report success while the bare process keeps running); a
// simply-stopped service (no providers discovered) still routes.
func TestOpenRCLifecycleFallsThroughToBareProvider(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
	discoverSystemdFn = func() []Provider { return []Provider{{User: "alice", PID: 777}} }

	for _, verb := range []string{"start", "stop", "restart"} {
		handled, err := openrcRouteLifecycle(verb, nil, true, false)
		if handled || err != nil {
			t.Fatalf("%s with only a bare provider: handled=%v err=%v, want false,nil", verb, handled, err)
		}
	}
	if rig.ran() != 0 {
		t.Fatalf("rc-service must not run for a sole bare provider; argv=%v", rig.argv)
	}

	// No providers discovered: a simply-stopped service still routes.
	discoverSystemdFn = func() []Provider { return nil }
	handled, err := openrcRouteLifecycle("start", nil, false, false)
	if !handled || err != nil {
		t.Fatalf("start with no providers: handled=%v err=%v, want true,nil", handled, err)
	}
	if got := strings.Join(rig.argv[len(rig.argv)-1], " "); got != "rc-service urnetwork start" {
		t.Fatalf("argv = %q, want the service start", got)
	}
}

// TestOpenRCPausedEntrySweptOnEnable: a .installer-paused leftover (even in
// the target queue) is removed when auto-update is (re-)enabled, so a stale
// file can never be executed by run-parts.
func TestOpenRCPausedEntrySweptOnEnable(t *testing.T) {
	newOpenRCTestRig(t, true, true)

	weeklyDir := filepath.Join(openrcPeriodicBaseDir, "weekly")
	dailyDir := filepath.Join(openrcPeriodicBaseDir, "daily")
	if err := os.MkdirAll(weeklyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dailyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pausedWeekly := filepath.Join(weeklyDir, openrcUpdateCronPausedName)
	pausedDaily := filepath.Join(dailyDir, openrcUpdateCronPausedName)
	for _, p := range []string{pausedWeekly, pausedDaily} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := openrcSetAutoUpdate("weekly", "/x/urnet-tools"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pausedWeekly); !os.IsNotExist(err) {
		t.Fatalf("paused leftover in the target queue survived enable (err=%v)", err)
	}
	if _, err := os.Stat(pausedDaily); !os.IsNotExist(err) {
		t.Fatalf("paused leftover in another queue survived enable (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(weeklyDir, openrcUpdateCronName)); err != nil {
		t.Fatalf("live entry missing after enable: %v", err)
	}
}

// TestRenderStatusBaseOpenRCStoppedService: with no resolved process (a
// stopped service) the rc-service view is still shown.
func TestRenderStatusBaseOpenRCStoppedService(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	if err := renderStatusBase(Provider{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range rig.argv {
		if strings.Join(a, " ") == "rc-service urnetwork status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rc-service status not run for a stopped service; argv = %v", rig.argv)
	}
}

// TestOpenRCHotSwapPreflightUnsupervisedOldVersion: the OpenRC decline is
// checked first, but an UNSUPERVISED provider with an unsupported version
// must still decline with the version reason (the reorder must not swallow
// it).
func TestOpenRCHotSwapPreflightUnsupervisedOldVersion(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSupervised })
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }

	err := hotSwapPreflight(Provider{Version: "v3.23.0-fix.20.0"})
	if !errors.Is(err, ErrHotSwapNotSupported) {
		t.Fatalf("hotSwapPreflight(unsupervised, old) = %v, want ErrHotSwapNotSupported", err)
	}
}

// TestOpenRCRouteElevationHint: a failed rc-service/rc-update run as a
// non-root user must carry the elevation hint.
func TestOpenRCRouteElevationHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("euid-based hint cannot be exercised as root")
	}
	newOpenRCTestRig(t, true, true)
	openrcRunFn = func(args ...string) error { return errors.New("exit status 1") }

	_, err := openrcRouteLifecycle("stop", nil, false, false)
	if err == nil || !strings.Contains(err.Error(), "re-run as root") {
		t.Fatalf("stop route error = %v, want the elevation hint", err)
	}
	_, err = openrcRouteAutoStart("on", nil, false, false)
	if err == nil || !strings.Contains(err.Error(), "re-run as root") {
		t.Fatalf("auto-start route error = %v, want the elevation hint", err)
	}
	if _, err := openrcRestartService(Provider{PID: 4242}); err == nil || !strings.Contains(err.Error(), "re-run as root") {
		t.Fatalf("restart error = %v, want the elevation hint", err)
	}
}

// TestRenderStatusBaseOpenRCView: the service's rc-service view is shown only
// when the resolved target is the service's supervised process.
func TestRenderStatusBaseOpenRCView(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)

	// Supervised provider: rc-service status is shown.
	providerSupervisedByOpenRCFn = func(p Provider) bool { return true }
	if err := renderStatusBase(Provider{PID: 123}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range rig.argv {
		if strings.Join(a, " ") == "rc-service urnetwork status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rc-service status not run; argv = %v", rig.argv)
	}

	// Bare provider: no service view.
	rig.argv = nil
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
	if err := renderStatusBase(Provider{PID: 123}); err != nil {
		t.Fatal(err)
	}
	if rig.ran() != 0 {
		t.Fatalf("bare provider got the service view: %v", rig.argv)
	}
}

// TestOpenRCRouteStopsRefuseAmbiguity is review finding R2: with the OpenRC
// service AND another provider on the box, a selector-less stop/restart must
// NOT silently act on the service alone and report success while the other
// provider keeps running. It must refuse like the systemd path does. `start` is
// not destructive and stays allowed; any explicit selector is honoured.
func TestOpenRCRouteStopsRefuseAmbiguity(t *testing.T) {
	svc := Provider{User: "urnet", StateDir: "/home/urnet/.urnetwork", Network: "net1", PID: 777, Running: true}
	other := Provider{User: "alice", StateDir: "/home/alice/.urnetwork", Network: "net2", PID: 4242, Running: true}

	for _, tc := range []struct {
		name     string
		verb     string
		provs    []Provider
		args     []string
		wantHand bool
		wantErr  bool
	}{
		{"stop refuses with a bare provider present", "stop", []Provider{svc, other}, nil, true, true},
		{"restart refuses with a bare provider present", "restart", []Provider{svc, other}, nil, true, true},
		{"start is allowed (not destructive)", "start", []Provider{svc, other}, nil, true, false},
		{"stop proceeds with only the service", "stop", []Provider{svc}, nil, true, false},
		{"explicit --user picks one provider", "stop", []Provider{svc, other}, []string{"--user", "urnet"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpenRCTestRig(t, true, true)
			provs := tc.provs
			discoverSystemdFn = func() []Provider { return provs }
			providerSupervisedByOpenRCFn = func(p Provider) bool { return p.PID == 777 }

			handled, err := openrcRouteLifecycle(tc.verb, tc.args, true, false)
			if handled != tc.wantHand {
				t.Fatalf("handled=%v want %v (err=%v)", handled, tc.wantHand, err)
			}
			if tc.wantErr && err == nil {
				t.Fatalf("expected refusal error, got nil (ran=%d)", rig.ran())
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && rig.ran() != 0 {
				t.Fatalf("refused command still acted on the service (%d rc calls)", rig.ran())
			}
		})
	}
}

// TestOpenRCStartWorksWithStoppedServiceDiscovered is the regression guard for
// a routing hole that made `urnet-tools start` fail on a real Alpine box with
// "no owning systemd unit".
//
// Stopped-service discovery reports the STOPPED service as a provider row
// (Supervisor "openrc", no pid, not running) so `urnet-tools providers` can
// list it. That row made len(provs) non-zero, and the old routing test only
// looked for a SUPERVISED (i.e. running) process among the rows - so a
// stopped service declined the OpenRC route and fell through to the systemd
// path, which cannot start it. A stopped service must route to rc-service.
func TestOpenRCStartWorksWithStoppedServiceDiscovered(t *testing.T) {
	rig := newOpenRCTestRig(t, true, true)
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }
	// The row discover_openrc.go produces for a stopped service.
	stopped := Provider{User: "urnet", StateDir: "/home/urnet/.urnetwork", Supervisor: "openrc"}
	discoverSystemdFn = func() []Provider { return []Provider{stopped} }

	for _, verb := range []string{"start", "stop", "restart"} {
		before := rig.ran()
		handled, err := openrcRouteLifecycle(verb, nil, true, false)
		if !handled || err != nil {
			t.Fatalf("%s with only the stopped service discovered: handled=%v err=%v, want true,nil", verb, handled, err)
		}
		if rig.ran() != before+1 {
			t.Fatalf("%s: rc-service did not run", verb)
		}
		if got := strings.Join(rig.argv[len(rig.argv)-1], " "); got != "rc-service urnetwork "+verb {
			t.Fatalf("%s: argv = %q", verb, got)
		}
	}

	// And the running service still routes.
	providerSupervisedByOpenRCFn = func(p Provider) bool { return p.PID == 4242 }
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "urnet", StateDir: "/home/urnet/.urnetwork", PID: 4242, Running: true}}
	}
	if handled, err := openrcRouteLifecycle("restart", nil, true, false); !handled || err != nil {
		t.Fatalf("running service: handled=%v err=%v, want true,nil", handled, err)
	}
}

// TestOpenRCCronGuardRootOwnedTree executes the generated auto-update entry
// and asserts which directory modes the root-ownership guard accepts.
//
// It runs as an ORDINARY user on purpose. The guard's logic is pure shell
// arithmetic over `stat -c %u` / `stat -c %a`, so the real question is not
// "can this process own a root-owned directory" but "does the generated
// `$((0$mode & 022))` evaluate an octal string as octal". A fake `stat` on
// PATH answers for the ownership and mode questions, which is exactly what
// the guard reads, and lets the test run everywhere — including GitHub
// Actions runners, which are non-root. The previous version skipped unless
// euid==0 and built its tree under /root, so on CI it never ran at all and the
// bug class it was written for (B1: `755` parsed as decimal against octal 022)
// had no guard at all.
func TestOpenRCCronGuardRootOwnedTree(t *testing.T) {
	binDir := t.TempDir()
	// The fake stat answers for every path, so the walk up to "/" sees the
	// same owner/mode for each component. `mode` is rewritten per case below
	// through the FAKE_STAT_MODE file, which keeps the shim a plain shell
	// script with no per-case arguments.
	fakeStat := filepath.Join(binDir, "stat")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  -c) case \"$2\" in\n" +
		"        %u) echo 0 ;;\n" +
		"        %a) cat \"$FAKE_STAT_MODE\" ;;\n" +
		"      esac ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeStat, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	modeFile := filepath.Join(binDir, "mode")
	if err := os.WriteFile(modeFile, []byte("755\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	tool := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n[ \"$*\" = 'update -f' ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		mode string
		safe bool
	}{
		// 755 is the mode the installer actually creates. It is the case B1
		// broke: decimal 755 & octal 022 = 18, so the guard refused it.
		{"0755", "755", true},
		{"0700", "700", true},
		{"0750", "750", true},
		{"0775", "775", false},
		{"0777", "777", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(modeFile, []byte(tc.mode+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", openrcUpdateCronScript(tool))
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+":"+os.Getenv("PATH"),
				"FAKE_STAT_MODE="+modeFile,
			)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.safe {
				t.Fatalf("mode %s: safe=%v but guard returned %v: %s", tc.mode, tc.safe, err, out)
			}
		})
	}
}

func TestCmdLogsOpenRCTarget(t *testing.T) {
	oldProcesses, oldStopped := discoverProcessesFn, discoverStoppedFn
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn = oldProcesses, oldStopped })
	for _, tc := range []struct {
		name       string
		provider   Provider
		supervised bool
		want       string
	}{
		{"stopped service", Provider{Supervisor: "openrc"}, false, "OpenRC service"},
		{"live service", Provider{PID: 4242}, true, "OpenRC service"},
		{"bare provider", Provider{}, false, "no systemd unit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newOpenRCTestRig(t, true, true)
			// Point both logs at nonexistent fixture paths so the service route returns
			// its diagnostic without starting tail or reading real service logs.
			missing := filepath.Join(t.TempDir(), "missing")
			content := "output_log=" + shellQuoteSingle(missing) + "\nerror_log=" + shellQuoteSingle(missing) + "\n"
			if err := os.WriteFile(openrcInitScriptPath, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
			p := tc.provider
			p.User = currentUserName()
			p.StateDir = t.TempDir()
			discoverProcessesFn = func() []Provider { return []Provider{p} }
			discoverStoppedFn = func([]Provider) []Provider { return nil }
			providerSupervisedByOpenRCFn = func(Provider) bool { return tc.supervised }
			if err := cmdLogs(nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cmdLogs = %v, want %q", err, tc.want)
			}
		})
	}
}
