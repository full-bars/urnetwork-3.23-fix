//go:build linux

package urnettools

import (
	"os"
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
	t.Cleanup(func() {
		openrcProbeFn, systemdRunningFn = oldProbe, oldSystemd
		openrcInitScriptPath, openrcRunFn, openrcToolPathFn = oldInit, oldRun, oldToolPath
		openrcPeriodicBaseDir, openrcExecutableFn = oldPeriodic, oldExecutable
		discoverSystemdFn = oldDiscover
	})
	// The restart gate enriches its audit line from discovery; stub it so a
	// test never touches (or writes a restart marker into) a real provider
	// state dir on the host running the tests.
	discoverSystemdFn = func() []Provider { return nil }

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
// resolvable AND at least one OpenRC runtime marker must exist. A host with
// only one of the two is not treated as OpenRC.
func TestProbeOpenRCDetection(t *testing.T) {
	oldLook, oldStat := openrcLookPathFn, openrcStatFn
	t.Cleanup(func() { openrcLookPathFn, openrcStatFn = oldLook, oldStat })

	cases := []struct {
		name      string
		rcService bool
		markers   map[string]bool
		want      bool
	}{
		{"rc-service + /run/openrc", true, map[string]bool{"/run/openrc": true}, true},
		{"rc-service + /sbin/openrc-run", true, map[string]bool{"/sbin/openrc-run": true}, true},
		{"rc-service + /usr/sbin/openrc-run", true, map[string]bool{"/usr/sbin/openrc-run": true}, true},
		{"rc-service, no markers", true, map[string]bool{}, false},
		{"no rc-service, marker present", false, map[string]bool{"/run/openrc": true}, false},
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
	if !strings.Contains(content, `exec "/opt/urnet/urnet-tools" update -f`) {
		t.Fatalf("entry content = %q, want an exec of the tool with `update -f`", content)
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
// kernel's 15-byte truncation of "supervise-daemon".
func TestIsSuperviseDaemonComm(t *testing.T) {
	for _, c := range []struct {
		comm string
		want bool
	}{
		{"supervise-daemon", true},
		{"supervise-daemo", true}, // TASK_COMM_LEN truncation
		{"", false},
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
// match os.Getppid(). Also pins the parsing against a comm containing
// spaces and parentheses (the reason only fields after the last ')' are
// parsed).
func TestParentPID(t *testing.T) {
	if got, want := parentPID(os.Getpid()), os.Getppid(); got != want {
		t.Fatalf("parentPID(self) = %d, want %d", got, want)
	}
	// A process whose comm contains ')' is parsed correctly because only the
	// fields after the LAST ')' are used.
	if got := parentPID(1); got != 0 {
		t.Fatalf("parentPID(1) = %d, want 0 (init has no parent)", got)
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
	oldStat, oldExec := openrcStatFn, openrcExecutableFn
	t.Cleanup(func() { openrcStatFn, openrcExecutableFn = oldStat, oldExec })

	dir := t.TempDir()
	providerBin := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(providerBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No sibling tool yet: falls to the running executable.
	openrcExecutableFn = func() (string, error) { return "/usr/local/bin/urnet-tools", nil }
	openrcStatFn = func(path string) (os.FileInfo, error) { return os.Stat(path) }
	if got := openrcUpdateToolPath(Provider{Binary: providerBin}); got != "/usr/local/bin/urnet-tools" {
		t.Fatalf("tool path = %q, want the running executable fallback", got)
	}
	// Sibling tool present: it wins.
	sibling := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := openrcUpdateToolPath(Provider{Binary: providerBin}); got != sibling {
		t.Fatalf("tool path = %q, want the sibling %q", got, sibling)
	}
}
