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
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Keep resolveDefaultProvider (persisted default provider config) from
	// reading the developer's real ~/.config during tests.
	os.Unsetenv("URNET_TOOLS_DEFAULT_PROVIDER")
	_ = os.Setenv("HOME", os.TempDir())
	_ = os.Setenv("XDG_CONFIG_HOME", os.TempDir())
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
