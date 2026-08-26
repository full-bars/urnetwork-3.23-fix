package urnettools

// Tests for lifecycleCandidates (explicit-target-only merged discovery).
//
// Contract under test:
//   - No explicit target: the candidate pool is Discover() ALONE — every
//     default-selection path behaves exactly as before this change, even on
//     boxes running docker containers alongside host providers.
//   - Explicit flag target (--unit/--user/--network/--network-id/--state-dir):
//     docker containers join the pool so a mistyped/mistargeted container
//     name gets the actionable guardSystemdProvider refusal (with the
//     urnet-docker hint) instead of a plain not-found.
//
// All commands run with dryRun=true: selection + guards execute for real,
// but nothing is started/stopped/restarted and no systemctl is invoked.

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Isolate every platform-specific config location this package reads,
	// so no developer/runner state leaks into selection behavior:
	//   unix:  $HOME + $XDG_CONFIG_HOME (UserHomeDir/UserConfigDir)
	//   win:   %USERPROFILE% (UserHomeDir) + %APPDATA% (UserConfigDir)
	// Without the Windows pair, os.UserConfigDir() still sees the runner's
	// real AppData and a stray persisted default silently changes the
	// selection path (seen as a spurious CI failure).
	os.Unsetenv("URNET_TOOLS_DEFAULT_PROVIDER")
	tmp := os.TempDir()
	_ = os.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		_ = os.Setenv("USERPROFILE", tmp)
		_ = os.Setenv("APPDATA", tmp)
	} else {
		_ = os.Setenv("XDG_CONFIG_HOME", tmp)
	}
	os.Exit(m.Run())
}

// stubDiscovery swaps both discovery seams for the duration of one test.
func stubDiscovery(t *testing.T, systemd, docker []Provider) {
	t.Helper()
	origS, origD := discoverSystemdFn, discoverDockerFn
	discoverSystemdFn = func() []Provider { return systemd }
	discoverDockerFn = func() []Provider { return docker }
	t.Cleanup(func() {
		discoverSystemdFn = origS
		discoverDockerFn = origD
	})
}

// stubPrivileged pins the privilege seam.
func stubPrivileged(t *testing.T, privileged bool) {
	t.Helper()
	orig := isPrivileged
	isPrivileged = func() bool { return privileged }
	t.Cleanup(func() { isPrivileged = orig })
}

// captureStdout comes from status_panel_test.go (same package).

var (
	hostProvider = Provider{User: "user", Unit: "urnetwork.service", Network: "testnet", StateDir: "/home/user/.urnetwork"}
	dockerProv   = Provider{User: "docker:ps", Unit: "ps", StateDir: "/root/.urnetwork"}
	secondHost   = Provider{User: "otheruser", Unit: "provider_beta-custom", Network: "n2", StateDir: "/home/otheruser/.urnetwork"}
)

// --- lifecycleCandidates pool construction ---

func TestLifecycleCandidates_NoTargetPoolExcludesContainers(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider, secondHost}, []Provider{dockerProv})

	got := lifecycleCandidates(Target{})
	if len(got) != 2 {
		t.Fatalf("no-target pool must contain ONLY systemd providers, got %d: %+v", len(got), got)
	}
	for _, p := range got {
		if isDockerProvider(p) {
			t.Fatalf("container leaked into no-target pool: %+v", p)
		}
	}
}

func TestLifecycleCandidates_ExplicitTargetWidensWithContainers(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})

	got := lifecycleCandidates(Target{Unit: "ps"})
	if len(got) != 2 {
		t.Fatalf("explicit-target pool should widen to systemd+docker, got %d: %+v", len(got), got)
	}
	found := false
	for _, p := range got {
		if isDockerProvider(p) {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit-target pool is missing the docker container")
	}
}

func TestHasExplicitTarget(t *testing.T) {
	cases := []struct {
		t    Target
		want bool
	}{
		{Target{}, false},
		{Target{Unit: "x"}, true},
		{Target{User: "x"}, true},
		{Target{Network: "x"}, true},
		{Target{NetworkID: "x"}, true},
		{Target{StateDir: "x"}, true},
	}
	for _, c := range cases {
		if got := hasExplicitTarget(c.t); got != c.want {
			t.Fatalf("hasExplicitTarget(%+v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// --- end-to-end through cmdStop / cmdRestart (dry-run) ---

func TestLifecycleCmds_ExplicitContainerTargetRefusedNotActedOn(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
	}{
		{"start", []string{"--unit", "ps"}},
		{"stop", []string{"--unit", "ps"}},
		{"restart", []string{"--unit", "ps"}},
		{"stop", []string{"--user", "docker:ps"}},
	} {
		t.Run(tc.cmd+"_"+strings.Join(tc.args, "_"), func(t *testing.T) {
			stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})
			var err error
			out := captureStdout(t, func() {
				err = cmdLifecycleForTest(tc.cmd, tc.args)
			})
			if err == nil {
				t.Fatal("expected docker-refusal error, got nil")
			}
			if !strings.Contains(err.Error(), "docker container") || !strings.Contains(err.Error(), "urnet-docker") {
				t.Fatalf("refusal should name the namespace + tool, got: %v", err)
			}
			if strings.Contains(out, "would") {
				t.Fatalf("command must not proceed past the guard, output: %q", out)
			}
		})
	}
}

// cmdLifecycleForTest dispatches to the lifecycle command by name (tests
// cannot import the cobra layer without dragging in flag plumbing).
func cmdLifecycleForTest(cmd string, args []string) error {
	switch cmd {
	case "start":
		return cmdStart(args, false, true)
	case "stop":
		return cmdStop(args, false, true)
	case "restart":
		return cmdRestart(args, false, true)
	}
	panic("unknown cmd " + cmd)
}

func TestLifecycleCmds_NoTargetOnMixedBoxPicksSystemdOnly(t *testing.T) {
	cur := currentUserName()
	mine := Provider{User: cur, Unit: "urnetwork.service", Network: "mynet", StateDir: "/home/x/.urnetwork", Running: true}
	stubDiscovery(t, []Provider{mine}, []Provider{dockerProv})
	stubPrivileged(t, false)

	var err error
	out := captureStdout(t, func() {
		err = cmdStop(nil, false, true)
	})
	if err != nil {
		t.Fatalf("no-target stop on a mixed box must behave exactly like before (pick the sole systemd provider): %v", err)
	}
	if !strings.Contains(out, "urnetwork.service") {
		t.Fatalf("should have targeted the host provider, output: %q", out)
	}
	if strings.Contains(out, "ps") {
		t.Fatalf("container must never be auto-selected: %q", out)
	}
}

func TestLifecycleCmds_RootNoTargetAmbiguityCountUnchangedByContainers(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider, secondHost}, []Provider{dockerProv})
	stubPrivileged(t, true)

	err := cmdRestart(nil, false, true)
	if err == nil {
		t.Fatal("root + multiple providers + no target must still refuse")
	}
	if !strings.Contains(err.Error(), "2 providers found") {
		t.Fatalf("inventory must list only the 2 systemd providers (containers excluded from no-target pool), got: %v", err)
	}
}

func TestLifecycleCmds_BarePositionalStillHardErrorsWithDockerHint(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})

	err := cmdStop([]string{"ps"}, false, true)
	if err == nil {
		t.Fatal("bare positional must remain a hard error")
	}
	if !strings.Contains(err.Error(), "urnet-docker stop ps") {
		t.Fatalf("positional error should carry the docker hint, got: %v", err)
	}
}

func TestLifecycleCmds_ExplicitUnknownTargetStillPlainNotFound(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})

	err := cmdStop([]string{"--unit", "does-not-exist"}, false, true)
	if err == nil {
		t.Fatal("unknown explicit target must error")
	}
	if !strings.Contains(err.Error(), "matches no running provider") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestLifecycleCmds_SystemdTargetUnaffectedByDockerPresence(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})

	// Selection + guard must pass; the dry-run plan itself is printed on
	// stderr by confirmGate, so only the outcome is asserted here.
	if err := cmdRestart([]string{"--unit", "urnetwork.service"}, false, true); err != nil {
		t.Fatalf("legitimate systemd target must keep working: %v", err)
	}
}

// --- commit 2: the other unit-driving commands get the same guard ---

// TestOtherSystemdCmds_ExplicitContainerTargetRefused pins that hot-restart,
// session save/load, auto-start, auto-update, uninstall and reinstall all
// refuse an explicit docker target via guardSystemdProvider (previously they
// returned a plain not-found — same latent class as the lifecycle HIGH).
func TestOtherSystemdCmds_ExplicitContainerTargetRefused(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})
	stubPrivileged(t, false)

	cases := []struct {
		name string
		run  func() error
	}{
		{"hot-restart", func() error { return cmdHotRestart([]string{"--unit", "ps"}, true, true) }},
		{"session", func() error { return cmdSession([]string{"save", "/tmp/x.tgz", "--unit", "ps"}) }},
		{"auto-start", func() error { return cmdAutoStart([]string{"on", "--unit", "ps"}, false, true) }},
		{"auto-update", func() error { return cmdAutoUpdate([]string{"daily", "--unit", "ps"}, false, true) }},
		{"uninstall", func() error { return cmdUninstall([]string{"--unit", "ps"}, true, true) }},
		{"reinstall", func() error { return cmdReinstall([]string{"--unit", "ps"}, true, true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s: expected docker refusal for --unit ps, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "docker container") || !strings.Contains(err.Error(), "urnet-docker") {
				t.Fatalf("%s: expected namespace refusal, got: %v", tc.name, err)
			}
		})
	}
}

// TestSessionSaveLegitimateSystemdTargetStillWorks guards against the
// over-refusal regression: a real systemd provider must sail through
// selection + guard (fails later on the missing file, which is fine).
func TestSessionSaveLegitimateSystemdTargetStillWorks(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, []Provider{dockerProv})

	err := cmdSession([]string{"save", "/tmp/x.tgz", "--unit", "urnetwork.service"})
	if err == nil {
		t.Fatal("missing session file should still fail")
	}
	if strings.Contains(err.Error(), "docker container") {
		t.Fatalf("legitimate systemd target must not be refused by the docker guard: %v", err)
	}
}

// --- Sonnet review round: additional coverage (reconciled set) ---

// Multiple containers must EACH be selectable by their unique docker:user
// value and refused (DiscoverDocker guarantees unique Users since
// User == "docker:"+container-name, so true user-ambiguity among containers
// cannot arise; this pins the per-container refusal instead).
func TestLifecycleCandidates_ContainerUserTargetRefusesEach(t *testing.T) {
	c1 := Provider{User: "docker:web1", Unit: "web1", StateDir: "/root/.urnetwork"}
	c2 := Provider{User: "docker:web2", Unit: "web2", StateDir: "/root/.urnetwork"}
	stubDiscovery(t, []Provider{hostProvider}, []Provider{c1, c2})

	for _, u := range []string{"docker:web1", "docker:web2"} {
		err := cmdStop([]string{"--user", u}, false, true)
		if err == nil || !strings.Contains(err.Error(), "docker container") {
			t.Fatalf("--user %s: expected namespace refusal, got: %v", u, err)
		}
	}
}

// Host provider and container sharing a unit name must produce the
// AMBIGUITY error, never a silent pick of either side (a silent pick of the
// host would bypass the operator's stated intent; of the container, the
// guard). Catches future dedupe/filter regressions between the pools.
func TestLifecycleCmds_HostAndContainerShareUnitNameIsAmbiguous(t *testing.T) {
	host := hostProvider
	host.Unit = "ps"
	stubDiscovery(t, []Provider{host}, []Provider{dockerProv})

	err := cmdStop([]string{"--unit", "ps"}, false, true)
	if err == nil {
		t.Fatal("shared unit name must be ambiguous")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got: %v", err)
	}
	if strings.Contains(err.Error(), "docker container") {
		t.Fatalf("ambiguity must surface BEFORE any namespace refusal, got: %v", err)
	}
}

// The widening claim covers ALL five target flags — exercise the remaining
// three end-to-end against a matched container.
func TestLifecycleCmds_RemainingFlagsMatchContainerRefused(t *testing.T) {
	withNet := dockerProv
	withNet.NetworkID = "abc123"
	stubDiscovery(t, []Provider{hostProvider}, []Provider{withNet})

	cases := []struct {
		name string
		args []string
	}{
		{"network-id", []string{"--network-id", "abc123"}},
		{"state-dir", []string{"--state-dir", "/root/.urnetwork"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdRestart(tc.args, false, true)
			if err == nil || !strings.Contains(err.Error(), "docker container") {
				t.Fatalf("%s targeting a container must refuse, got: %v", tc.name, err)
			}
		})
	}
}

// Docker CLI absent (DiscoverDocker -> nil) on an explicit unknown target:
// plain not-found, no panic — pins the real-world "no docker on this host"
// shape distinct from the tested "docker present" shape.
func TestLifecycleCandidates_DockerCLIAbsentExplicitTargetCleanFallthrough(t *testing.T) {
	stubDiscovery(t, []Provider{hostProvider}, nil)

	err := cmdStop([]string{"--unit", "ghost"}, false, true)
	if err == nil {
		t.Fatal("unknown target must still error")
	}
	if !strings.Contains(err.Error(), "matches no running provider") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

// On the narrowed auto-pick path the printed count comes from the POOL SIZE:
// it must stay the systemd-only count even when containers exist on the box.
func TestLifecycleCmds_NarrowedNoteCountExcludesDocker(t *testing.T) {
	cur := currentUserName()
	mine := Provider{User: cur, Unit: "urnetwork.service", Network: "mynet", StateDir: "/home/x/.urnetwork", Running: true}
	others := Provider{User: "otheruser", Unit: "provider_beta-custom", Network: "n2", StateDir: "/home/o/.urnetwork", Running: true}
	stubDiscovery(t, []Provider{mine, others}, []Provider{dockerProv})
	stubPrivileged(t, false)

	var out string
	err := func() (err error) {
		out = captureStdout(t, func() { err = cmdStop(nil, false, true) })
		return err
	}()
	if err != nil {
		t.Fatalf("sole-accessible auto-pick should succeed: %v", err)
	}
	if !strings.Contains(out, "2 providers found") || strings.Contains(out, "3 providers found") {
		t.Fatalf("narrowed note must count the systemd-only pool (2), got: %q", out)
	}
}

// Documents today's UX on a containers-only box (Sonnet LOW finding #1):
// no-target start says "no providers found on this box" WITHOUT a docker
// hint. Pinning it so a future hint here is a deliberate decision.
func TestLifecycleCmds_EmptySystemdBoxNoTargetNoDockerHint(t *testing.T) {
	stubDiscovery(t, nil, []Provider{dockerProv})

	err := cmdStart(nil, false, true)
	if err == nil {
		t.Fatal("empty box must error")
	}
	if !strings.Contains(err.Error(), "no providers found on this box") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "urnet-docker") {
		t.Fatalf("no-target empty-box error must stay hint-free until deliberately changed: %v", err)
	}
}
