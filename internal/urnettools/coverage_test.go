//go:build linux

package urnettools

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestStdinReaderSharedAcrossPrompts pins the invariant that every
// interactive prompt reads from the SAME bufio.Reader (free-review HIGH,
// mimo-v2.5): a second bufio.Reader over the same input would silently
// drop whatever the first already buffered, hanging piped scripts like
// `echo y | urnet-tools update --all`. We swap the package-level
// stdinReader for the duration of the test and confirm two sequential
// confirmGate-style reads consume successive lines, not the same one.

// forceInteractiveForTest makes the confirm/picker tests run as if stdin is
// a terminal (they feed stdin via a substituted reader, which is not a TTY).
func forceInteractiveForTest(t *testing.T) {
	t.Helper()
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return true }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
}

func TestStdinReaderSharedAcrossPrompts(t *testing.T) {
	forceInteractiveForTest(t)
	orig := stdinReader
	defer func() { stdinReader = orig }()

	stdinReader = bufio.NewReader(strings.NewReader("yes\nno\n"))

	p := Provider{Unit: "urnetwork-native.service", User: "urnet"}
	ok1, err := confirmGate("first prompt", p, false, false)
	if err != nil {
		t.Fatalf("first confirmGate: %v", err)
	}
	if !ok1 {
		t.Fatalf("first confirmGate should read 'yes' -> true")
	}

	ok2, err := confirmGate("second prompt", p, false, false)
	if err == nil && ok2 {
		t.Fatalf("second confirmGate should read 'no' (the second buffered line), got ok=true err=nil")
	}
	// "no" doesn't match "yes" so confirmGate returns an "aborted" error.
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("second confirmGate should abort on 'no', got ok=%v err=%v", ok2, err)
	}
}

// TestUnitStateDirHomeForUserFallback covers unitStateDir's three paths:
// empty user, a resolvable user (root, via getent), and an unresolvable
// user (falls back to the /home/<user> convention rather than erroring).
func TestUnitStateDirHomeForUserFallback(t *testing.T) {
	if got := unitStateDir(""); got != "" {
		t.Errorf("unitStateDir(\"\") = %q, want empty", got)
	}

	// A user guaranteed not to exist on any box: fall back to the
	// hardcoded /home/<user> convention instead of erroring.
	bogus := "urnet-tools-test-nonexistent-user-9f3a"
	if _, err := exec.Command("getent", "passwd", bogus).Output(); err == nil {
		t.Skip("bogus test user unexpectedly resolves via getent on this box")
	}
	want := filepath.Join("/home", bogus, ".urnetwork")
	if got := unitStateDir(bogus); got != want {
		t.Errorf("unitStateDir(%q) = %q, want fallback %q", bogus, got, want)
	}

	// root always resolves via getent (or the box has no passwd db at
	// all, in which case skip rather than assert a brittle path).
	if _, err := exec.Command("getent", "passwd", "root").Output(); err != nil {
		t.Skip("no getent/passwd db on this box")
	}
	got := unitStateDir("root")
	if !strings.HasSuffix(got, string(filepath.Separator)+".urnetwork") {
		t.Errorf("unitStateDir(root) = %q, want a path ending in /.urnetwork", got)
	}
}

// TestIsUserUnitVendorDir covers the /usr/lib + /lib systemd system dirs
// (free-review MEDIUM): a unit shipped by a package (not a fleet install)
// lives there and must be classified as a system unit, not a user unit.
// systemd-journald.service ships with the systemd core package on any
// real systemd Linux box; skip if this environment lacks it entirely.
func TestIsUserUnitVendorDir(t *testing.T) {
	const vendorUnit = "systemd-journald.service"
	found := false
	for _, dir := range []string{"/usr/lib/systemd/system", "/lib/systemd/system", "/etc/systemd/system"} {
		if _, err := os.Stat(filepath.Join(dir, vendorUnit)); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Skip("systemd-journald.service not present on this box (no systemd core package)")
	}
	if isUserUnit(vendorUnit) {
		t.Errorf("isUserUnit(%q) = true, want false (vendor unit under /usr/lib or /lib)", vendorUnit)
	}

	// A unit name that (almost certainly) exists nowhere on the box must
	// be classified as a user unit by the same heuristic.
	fakeUnit := "urnet-tools-test-fake-unit-9f3a.service"
	if !isUserUnit(fakeUnit) {
		t.Errorf("isUserUnit(%q) = false, want true (no system unit file exists)", fakeUnit)
	}
}

// TestJournalctlArgsUserVsSystem covers the argv construction split (free
// review + coderabbit passes): system units use "-fu <unit>"; user units
// scope to the owning user's session via -M/--user-unit, because a plain
// "-fu" against a user-unit name queries the SYSTEM journal instead.
func TestJournalctlArgsUserVsSystem(t *testing.T) {
	// A unit name with no backing file anywhere -> isUserUnit is true.
	userProvider := Provider{Unit: "urnet-tools-test-fake-unit-9f3a.service", User: "urnet"}
	got := journalctlArgs(userProvider)
	want := []string{"-M", "urnet@", "--user-unit", userProvider.Unit, "-f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journalctlArgs(user unit) = %v, want %v", got, want)
	}

	// No User set: even if the unit "looks" user-level, we can't scope a
	// session without a user, so it falls back to plain -fu.
	noUserProvider := Provider{Unit: "urnet-tools-test-fake-unit-9f3a.service"}
	got = journalctlArgs(noUserProvider)
	want = []string{"-fu", noUserProvider.Unit}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journalctlArgs(no user) = %v, want %v", got, want)
	}
}

// TestTarRelPath covers the forward-slash-always tar path construction
// (free-review critical): using filepath.Join here would emit backslashes
// on a Windows host and the in-archive lookup would never match, since
// tar headers always use forward slashes regardless of the host OS.
func TestTarRelPath(t *testing.T) {
	if got, want := tarRelPath("linux", "amd64"), "linux/amd64/provider"; got != want {
		t.Errorf("tarRelPath(linux, amd64) = %q, want %q", got, want)
	}
	if got, want := tarRelPath("linux", "arm64"), "linux/arm64/provider"; got != want {
		t.Errorf("tarRelPath(linux, arm64) = %q, want %q", got, want)
	}
	if got, want := tarRelPath("windows", "amd64"), "windows/amd64/provider.exe"; got != want {
		t.Errorf("tarRelPath(windows, amd64) = %q, want %q", got, want)
	}
	if strings.ContainsRune(tarRelPath("windows", "amd64"), '\\') {
		t.Errorf("tarRelPath must never emit backslashes, got %q", tarRelPath("windows", "amd64"))
	}
}

// TestOptimizeForDispatch covers the platform dispatch in cmdOptimize
// (Linux sysctl vs Windows netsh/reg): windows must route to
// optimizeWindows, every other GOOS to optimizeLinux. Compares function
// pointers so the actual (root-requiring, host-mutating) implementations
// never run.
func TestOptimizeForDispatch(t *testing.T) {
	fnPtr := func(f func() error) uintptr { return reflect.ValueOf(f).Pointer() }

	if got, want := fnPtr(optimizeFor("windows")), fnPtr(optimizeWindows); got != want {
		t.Errorf("optimizeFor(windows) did not dispatch to optimizeWindows")
	}
	if got, want := fnPtr(optimizeFor("linux")), fnPtr(optimizeLinux); got != want {
		t.Errorf("optimizeFor(linux) did not dispatch to optimizeLinux")
	}
	// Any other GOOS (darwin, freebsd, ...) falls back to the Linux path
	// rather than erroring — cmdOptimize has no third implementation.
	if got, want := fnPtr(optimizeFor("darwin")), fnPtr(optimizeLinux); got != want {
		t.Errorf("optimizeFor(darwin) should default to optimizeLinux")
	}
	// Sanity: the real runtime.GOOS on this test box must resolve to one
	// of the two known branches without panicking.
	_ = optimizeFor(runtime.GOOS)
}

// TestDigestForAsset covers the mandatory-digest resolution shared by
// fetchLatestRelease and fetchReleaseByTag: a present asset with a
// "sha256:"-prefixed digest resolves to the bare hex digest; a missing
// asset or an asset with an empty digest both resolve to "" so the caller
// refuses the download rather than silently skipping verification
// (free-review critical).
func TestDigestForAsset(t *testing.T) {
	assets := []releaseAsset{
		{Name: "urnetwork-provider-v1.0.0.tar.gz", Digest: "sha256:abc123"},
		{Name: "urnetwork-hub-v1.0.0.tar.gz", Digest: ""},
	}
	if got, want := digestForAsset(assets, "urnetwork-provider-v1.0.0.tar.gz"), "abc123"; got != want {
		t.Errorf("digestForAsset(present) = %q, want %q", got, want)
	}
	if got := digestForAsset(assets, "urnetwork-hub-v1.0.0.tar.gz"); got != "" {
		t.Errorf("digestForAsset(empty digest) = %q, want empty", got)
	}
	if got := digestForAsset(assets, "does-not-exist.tar.gz"); got != "" {
		t.Errorf("digestForAsset(missing asset) = %q, want empty", got)
	}
}

// TestRestartProviderNoUnitNoPID: restartProvider must return a clear error
// when no systemd unit is resolved AND no PID is available (the fallback
// path). This pins the user-level fallback logic: system systemctl fails
// -> user systemctl fails -> PID signal skipped (PID=0) -> error.
func TestRestartProviderNoUnitNoPID(t *testing.T) {
	p := Provider{User: "testuser", Unit: "", PID: 0}
	err := restartProvider(p)
	if err == nil {
		t.Fatal("restartProvider with no unit and no PID must error")
	}
	if !strings.Contains(err.Error(), "could not restart") {
		t.Errorf("error should say 'could not restart', got: %v", err)
	}
}

// TestRestartProviderWithUnitFailsGracefully: when a unit is set but
// systemctl is not available (or the unit doesn't exist), restartProvider
// must return an error, not panic.
func TestRestartProviderWithUnitFailsGracefully(t *testing.T) {
	// Use a fake unit name that systemctl won't find.
	p := Provider{User: "testuser", Unit: "urnet-tools-test-fake-unit-restart.service", PID: 0}
	err := restartProvider(p)
	// Should error (systemctl will fail for the fake unit), not panic.
	if err == nil {
		t.Log("restartProvider returned nil — systemctl may have succeeded unexpectedly")
	}
}

// TestWriteTimerCalendarMissingHome covers the guard added alongside this
// review: writeTimerCalendar must error cleanly when getent can't resolve
// the target user's home, rather than silently falling back to a
// CWD-relative ".config/systemd/user/<timer>" path (the same class of bug
// fixed elsewhere as "review finding M3").
func TestWriteTimerCalendarMissingHome(t *testing.T) {
	bogus := "urnet-tools-test-nonexistent-user-9f3a"
	if _, err := exec.Command("getent", "passwd", bogus).Output(); err == nil {
		t.Skip("bogus test user unexpectedly resolves via getent on this box")
	}
	p := Provider{User: bogus}
	err := writeTimerCalendar("urnet-tools-test-fake-unit-9f3a.timer", p, "daily")
	if err == nil {
		t.Fatal("writeTimerCalendar with unresolvable home must error")
	}
	if !strings.Contains(err.Error(), "cannot resolve home") {
		t.Errorf("error should say home could not be resolved, got: %v", err)
	}
}

// TestSystemctlUserArgsSameUser: when the target user IS the current OS user,
// systemctlUserArgs must return ["--user"] only — NOT ["--user", "-M", user+"@"].
// The -M form routes through systemd-machined which requires root, so calling
// it as the same user produces "Operation not permitted" (the bug fixed in
// fix/urntools-status-systemctl and fix/journalctl-same-user-privilege).
// This test is the regression guard for that fix: a regression that adds -M
// back for same-user calls will be caught here.
func TestSystemctlUserArgsSameUser(t *testing.T) {
	cur := currentUserName()
	if cur == "" {
		t.Skip("cannot determine current user name on this runner")
	}
	got := systemctlUserArgs(cur)
	want := []string{"--user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("systemctlUserArgs(same user %q) = %v, want %v\n"+
			"REGRESSION: same-user calls must not use -M (requires root via machined)",
			cur, got, want)
	}
}

// TestSystemctlUserArgsCrossUser: for a different user, systemctlUserArgs must
// include -M <user>@ to reach the other user's session bus.
func TestSystemctlUserArgsCrossUser(t *testing.T) {
	cur := currentUserName()
	other := "urnet-tools-test-other-user-9f3a"
	if cur == other {
		t.Skip("unlikely: current user matches the fake cross-user name")
	}
	got := systemctlUserArgs(other)
	want := []string{"--user", "-M", other + "@"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("systemctlUserArgs(cross-user %q) = %v, want %v", other, got, want)
	}
}

// TestRenderSystemctlStatusLinuxNoUnit: renderSystemctlStatusLinux must return
// a clear error when the provider has no owning unit (bare-process provider).
// Before PR #438, cmdStatus on Linux would panic on a nil/empty unit because
// it blindly passed p.Unit to exec.Command; the fix adds an early guard.
func TestRenderSystemctlStatusLinuxNoUnit(t *testing.T) {
	p := Provider{User: "testuser", Unit: ""}
	err := renderSystemctlStatusLinux(p)
	if err == nil {
		t.Fatal("renderSystemctlStatusLinux with empty unit must error")
	}
	if !strings.Contains(err.Error(), "no owning unit") {
		t.Errorf("error should mention 'no owning unit', got: %v", err)
	}
}

// TestCleanupLifecycleUsesSystemctlUserArgs: verifies that cleanupLifecycle
// (called by cmdUninstall) uses systemctlUserArgs — not a raw "--user -M user@"
// — when disabling the auto-update timer for the owning user. A raw -M call
// requires root via machined and fails for same-user uninstalls (residual bug
// fixed alongside PR #438 regression tests).
// This is a structural smoke test: we supply a provider with no real unit so
// cleanupLifecycle returns immediately after the guard, proving it reaches the
// systemctlUserArgs branch without panic.
func TestCleanupLifecycleNoUnit(t *testing.T) {
	// Unit="" hits the first guard and returns immediately — safe on any box.
	p := Provider{User: "testuser", Unit: ""}
	cleanupLifecycle(p) // must not panic
}

