//go:build linux

package urnettools

import (
	"os"
	"os/exec"
	"os/user"
	"testing"
	"time"
)

// TestDiscoverProcessesFallsBackToProcessOwnerWhenEnvironUnreadable
// exercises the real cross-user fallback in discoverProcesses(): when
// readEnviron fails (simulated — reading another user's
// /proc/<pid>/environ requires root/CAP_SYS_PTRACE and cannot be provoked
// for real in a single-user test sandbox), the scan must still identify the
// process owner via processOwner()'s /proc stat fallback instead of
// leaving User/StateDir blank — the exact ghost-row bug this fix closes.
// Prior tests only exercised the synthetic-list narrowing logic
// (narrowToAccessible on hand-built Provider slices), never the real
// discovery path that produces those rows.
func TestDiscoverProcessesFallsBackToProcessOwnerWhenEnvironUnreadable(t *testing.T) {
	me := currentUserName()
	if me == "" {
		t.Skip("could not resolve current username")
	}

	// A real child process so /proc/<pid> is a genuine, stat-able entry.
	// argv[0] is overridden to "provider" so isProviderArg matches it; the
	// actual binary run is still /bin/sleep.
	cmd := exec.Command("sleep", "30")
	cmd.Args = []string{"provider", "30"}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test child process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	origReadEnviron := readEnviron
	readEnviron = func(pid int) map[string]string { return nil }
	defer func() { readEnviron = origReadEnviron }()

	// cmd.Start() returns as soon as the fork succeeds, which on Linux can
	// precede the child's execve. Until that exec lands, /proc/<pid>/cmdline
	// still holds this test binary's own argv, isProviderArg rejects it, and
	// the scan correctly reports zero providers. Scanning once races the
	// exec and fails only under load (green locally, red on a busy CI
	// runner), so poll for the child to appear instead.
	var providers []Provider
	var found *Provider
	deadline := time.Now().Add(10 * time.Second)
	for {
		providers = discoverProcesses()
		for i := range providers {
			if providers[i].PID == cmd.Process.Pid {
				found = &providers[i]
				break
			}
		}
		if found != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if found == nil {
		t.Fatalf("discoverProcesses did not find the test child (pid %d) among %d providers", cmd.Process.Pid, len(providers))
	}
	if found.User != me {
		t.Errorf("User = %q, want %q — processOwner fallback should have resolved it via /proc stat", found.User, me)
	}
	if found.StateDir == "" {
		t.Error("StateDir is empty — processOwner fallback should have derived it from the resolved home directory")
	}
}

// TestSelectTargetOrSoleAccessibleNarrowsToOwnUser: unprivileged caller,
// multiple providers discovered, only one under the caller's own account —
// `logs` should auto-pick it instead of refusing with the ambiguity guard,
// since the other providers belong to accounts the caller has no way to
// select correctly anyway (systemd/journalctl cross-user access needs
// root).
func TestSelectTargetOrSoleAccessibleNarrowsToOwnUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can reach every provider; narrowing only applies unprivileged")
	}
	me := currentUserName()
	if me == "" {
		t.Skip("could not resolve current username")
	}
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Running: true},
		{User: me, Unit: "urnetwork-native.service", Running: true},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service", Running: true},
	}

	p, narrowed, err := selectTargetOrSoleAccessible(providers, Target{}, true)
	if err != nil {
		t.Fatalf("selectTargetOrSoleAccessible: %v", err)
	}
	if !narrowed {
		t.Error("expected narrowed=true when exactly one provider is the caller's own user")
	}
	if p.User != me {
		t.Errorf("selected provider user = %q, want %q", p.User, me)
	}
}

// TestSelectTargetOrSoleAccessibleDoesNotNarrowStopped: a provider that is the
// caller's own AND the sole accessible candidate must still NOT be auto-targeted
// when it is stopped — the sole-accessible path drives destructive stop/restart
// and must never pick a non-running provider.
func TestSelectTargetOrSoleAccessibleDoesNotNarrowStopped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can reach every provider; narrowing only applies unprivileged")
	}
	me := currentUserName()
	if me == "" {
		t.Skip("could not resolve current username")
	}
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Running: true},
		{User: me, Unit: "urnetwork-native.service", Running: false}, // stopped
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service", Running: true},
	}

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{}, true)
	if err == nil {
		t.Fatal("expected an error when the caller's sole own provider is stopped (no running sole candidate)")
	}
	if narrowed {
		t.Error("must not auto-narrow to a STOPPED provider for a destructive command (requireRunning=true)")
	}

	// Read-only callers (logs/status/summary) pass requireRunning=false and
	// MUST still reach a stopped provider for diagnostics.
	got, narrowed2, err2 := selectTargetOrSoleAccessible(providers, Target{}, false)
	if err2 != nil {
		t.Fatalf("read-only selection of a stopped provider should succeed: %v", err2)
	}
	if !narrowed2 {
		t.Error("read-only (requireRunning=false) should still narrow to the stopped sole provider")
	}
	if got.User != me {
		t.Errorf("read-only selected user %q, want %q", got.User, me)
	}
}

// TestSelectTargetOrSoleAccessibleStillRefusesWhenAmbiguous: two providers
// both under accounts the caller can't disambiguate (neither is "own user")
// must still hit the normal refusal — narrowing only resolves the case
// where exactly one candidate remains.
func TestSelectTargetOrSoleAccessibleStillRefusesWhenAmbiguous(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can reach every provider; narrowing only applies unprivileged")
	}
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Running: true},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service", Running: true},
	}

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{}, true)
	if err == nil {
		t.Fatal("expected refusal when no provider matches the caller's own user")
	}
	if narrowed {
		t.Error("narrowed should be false when the refusal path is taken")
	}
}

// TestSelectTargetOrSoleAccessibleTreatsBlankUserAsUnresolved: a blank
// p.User means processOwner's own owner lookup failed (e.g. a numeric UID
// with no matching passwd entry) — the owner is unknown, not unrestricted.
// It must not be auto-selected as if it belonged to the caller.
func TestSelectTargetOrSoleAccessibleTreatsBlankUserAsUnresolved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can reach every provider; narrowing only applies unprivileged")
	}
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service"},
		{User: "", Unit: "urnetwork-ghost.service"},
	}

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{}, true)
	if err == nil {
		t.Fatal("expected refusal when the only candidate has an unresolved owner")
	}
	if narrowed {
		t.Error("narrowed should be false; a blank-User row must not be auto-selected")
	}
}

// TestParseUnitLinesDedupesUnitPresentInBothListings: discoverSystemUnits
// and discoverUserUnits both merge `systemctl list-units --all` output with
// `systemctl list-unit-files` output (list-units misses never-started
// units that list-unit-files sees), by simple concatenation. Any unit that
// is loaded AND has a unit file on disk — the common case for an
// enabled-but-currently-stopped unit — appears in both listings and must be
// deduped by parseUnitLines, or it yields two identical Provider rows. Live
// fleet symptom (2026-08-17): every stopped unit doubled in `urnet-tools
// logs`'s ambiguity list.
func TestParseUnitLinesDedupesUnitPresentInBothListings(t *testing.T) {
	// list-units --all line, then a list-unit-files line for the SAME unit,
	// concatenated the way discoverSystemUnits builds `out`.
	text := "urnetwork-native.service loaded inactive dead urnetwork-native.service\n" +
		"urnetwork-native.service enabled\n"
	got := parseUnitLines(text, nil, func(string) string { return "urnet" }, nil)
	if len(got) != 1 {
		t.Fatalf("parseUnitLines returned %d providers, want 1 (unit appears in both listings): %+v", len(got), got)
	}
	if got[0].Unit != "urnetwork-native.service" {
		t.Errorf("Unit = %q, want %q", got[0].Unit, "urnetwork-native.service")
	}
}

// TestParseUnitLinesSkipsRunningAndNonProviderUnits verifies the two other
// filters parseUnitLines applies: a unit already backed by a running
// process is skipped (it's represented by that Provider already), and a
// non-provider unit name is skipped outright.
func TestParseUnitLinesSkipsRunningAndNonProviderUnits(t *testing.T) {
	running := []Provider{{Unit: "urnetwork-native.service", Running: true}}
	text := "urnetwork-native.service loaded active running\n" +
		"nginx.service loaded active running\n" +
		"provider-dashboard.service loaded active running\n"
	got := parseUnitLines(text, running, func(string) string { return "urnet" }, nil)
	if len(got) != 0 {
		t.Errorf("parseUnitLines returned %d providers, want 0 (running unit + non-provider units should all be excluded): %+v", len(got), got)
	}
}

// TestSelectTargetOrSoleAccessibleExplicitTargetBypassesNarrowing: an
// explicit --unit/--user always resolves strictly via selectTarget; the
// narrowing shortcut only kicks in for the no-target case.
func TestSelectTargetOrSoleAccessibleExplicitTargetBypassesNarrowing(t *testing.T) {
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service"},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service"},
	}

	p, narrowed, err := selectTargetOrSoleAccessible(providers, Target{Unit: "urnetwork-alpha.service"}, true)
	if err != nil {
		t.Fatalf("selectTargetOrSoleAccessible: %v", err)
	}
	if narrowed {
		t.Error("narrowed should be false when an explicit target was given")
	}
	if p.Unit != "urnetwork-alpha.service" {
		t.Errorf("selected unit = %q, want urnetwork-alpha.service", p.Unit)
	}
}

// TestProcessOwnerResolvesSelf verifies processOwner can identify the
// invoking process via /proc/<pid> stat — the fallback path used when
// /proc/<pid>/environ is unreadable for another user's process (permission
// denied without root/CAP_SYS_PTRACE). Using our own PID isn't a permission
// gap, but it does prove the stat + LookupId path resolves to the correct
// account, which is the mechanism the cross-user case depends on.
func TestProcessOwnerResolvesSelf(t *testing.T) {
	want, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}

	gotUser, gotHome := processOwner(os.Getpid())
	if gotUser != want.Username {
		t.Errorf("processOwner user = %q, want %q", gotUser, want.Username)
	}
	if gotHome != want.HomeDir {
		t.Errorf("processOwner home = %q, want %q", gotHome, want.HomeDir)
	}
}

// TestProcessOwnerInvalidPID verifies a nonexistent PID fails closed (empty
// strings) rather than panicking or returning stale data — Discover() must
// tolerate a process that exits between the /proc scan and this lookup.
func TestProcessOwnerInvalidPID(t *testing.T) {
	const bogusPID = 999999999
	gotUser, gotHome := processOwner(bogusPID)
	if gotUser != "" || gotHome != "" {
		t.Errorf("processOwner(%d) = (%q, %q), want (\"\", \"\")", bogusPID, gotUser, gotHome)
	}
}

// TestDiscoverProcessesFallsBackToProcessOwner reproduces the bug: when a
// provider process's environ can't supply USER/HOME (simulated here by
// exercising the same fallback discoverProcesses uses), the resulting
// Provider must still carry a usable User + StateDir instead of the blank,
// untargetable row the ghost-Provider bug produced (all fields empty except
// PID/Binary, which providerLabel and the "N providers found" listing never
// print).
func TestDiscoverProcessesFallsBackToProcessOwner(t *testing.T) {
	want, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}

	// Empty env mirrors readEnviron's nil return on a permission-denied
	// /proc/<pid>/environ read for another user's process.
	env := map[string]string{}
	stateDir := stateDirFor(env)
	if stateDir != "" {
		t.Fatalf("stateDirFor(empty env) = %q, want empty", stateDir)
	}

	ownerUser, ownerHome := processOwner(os.Getpid())
	if ownerUser == "" {
		t.Fatal("processOwner returned empty user for own PID")
	}
	if ownerUser != want.Username {
		t.Errorf("owner user = %q, want %q", ownerUser, want.Username)
	}
	if ownerHome == "" {
		t.Fatal("processOwner returned empty home for own PID")
	}
}

// TestParseExecStartPath: a stopped provider's Binary comes from the unit's
// ExecStart, and systemd renders that property as a bracketed record rather
// than a bare path. Getting this wrong leaves Provider.Binary empty, which
// makes `urnet-tools update` refuse every stopped provider with "no
// resolvable binary path — nothing updated" (CI shakedown 2026-09-08).
func TestParseExecStartPath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "systemd bracketed record",
			raw:  "{ path=/home/urnet/.local/share/urnetwork-provider/bin/urnetwork ; argv[]=/home/urnet/.local/share/urnetwork-provider/bin/urnetwork provide ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }",
			want: "/home/urnet/.local/share/urnetwork-provider/bin/urnetwork",
		},
		{
			name: "bare command line",
			raw:  "/usr/local/bin/urnetwork provide",
			want: "/usr/local/bin/urnetwork",
		},
		{
			name: "multiple ExecStart lines resolves to the first",
			raw:  "{ path=/opt/a/urnetwork ; argv[]=/opt/a/urnetwork provide }\n{ path=/opt/b/urnetwork ; argv[]=/opt/b/urnetwork provide }",
			want: "/opt/a/urnetwork",
		},
		{name: "empty", raw: "", want: ""},
		{name: "whitespace only", raw: "   \n  ", want: ""},
		{name: "relative path is not a resolvable binary", raw: "{ path=urnetwork ; argv[]=urnetwork provide }", want: ""},
		{name: "no path field and not absolute", raw: "{ argv[]=urnetwork provide }", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseExecStartPath(tc.raw); got != tc.want {
				t.Errorf("parseExecStartPath(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestProviderFromUnitCarriesBinary: providerFromUnit must propagate the
// resolved ExecStart into Provider.Binary even when the state dir is
// unresolvable (the early-return path), since update.go's precondition loop
// checks Binary regardless.
func TestProviderFromUnitCarriesBinary(t *testing.T) {
	p := providerFromUnit("urnetwork.service", "urnet", "/opt/urnetwork/bin/urnetwork")
	if p.Binary != "/opt/urnetwork/bin/urnetwork" {
		t.Errorf("Binary = %q, want /opt/urnetwork/bin/urnetwork", p.Binary)
	}
	if p.Running {
		t.Error("a unit-derived provider must not be marked Running")
	}
}

// TestIsProviderUnitRejectsNonServiceUnits: only a .service can be a
// provider. An ordinary install ships urnetwork-update.timer alongside the
// provider, and the name-only rule matched it, so `urnet-tools logs` listed
// timers as providers and the ambiguity guard then refused every command.
func TestIsProviderUnitRejectsNonServiceUnits(t *testing.T) {
	for _, unit := range []string{
		"urnetwork-update.timer",
		"urnetwork-sentinel-update.timer",
		"urnetwork.socket",
		"urnetwork.path",
		"urnetwork",
	} {
		if isProviderUnit(unit) {
			t.Errorf("isProviderUnit(%q) = true, want false: only a .service can be a provider", unit)
		}
	}
	for _, unit := range []string{"urnetwork.service", "urnetwork-native.service"} {
		if !isProviderUnit(unit) {
			t.Errorf("isProviderUnit(%q) = false, want true", unit)
		}
	}
}

// TestParseUnitLinesCorroboratesExecStart: a unit whose name matches the
// provider prefix but whose ExecStart runs something else is not a provider.
// Matching on name alone needs a deny-list of every sibling that shares the
// prefix, which is unbounded — provider-dashboard hit this on 2026-08-17 and
// urnetwork-sentinel on 2026-09-09, both flooding the candidate list and
// blocking auto-pick. ExecStart is evidence, so it decides when readable.
func TestParseUnitLinesCorroboratesExecStart(t *testing.T) {
	lines := "urnetwork.service loaded active running\n" +
		"urnetwork-sentinel.service loaded active running\n"

	binaries := map[string]string{
		"urnetwork.service":          "/home/klets/.local/share/urnetwork-provider/bin/urnetwork",
		"urnetwork-sentinel.service": "/usr/bin/python3",
	}

	got := parseUnitLines(lines, nil,
		func(string) string { return "klets" },
		func(unit string) string { return binaries[unit] },
	)

	if len(got) != 1 {
		names := []string{}
		for _, p := range got {
			names = append(names, p.Unit)
		}
		t.Fatalf("parseUnitLines returned %d providers (%v), want only urnetwork.service", len(got), names)
	}
	if got[0].Unit != "urnetwork.service" {
		t.Errorf("kept %q, want urnetwork.service", got[0].Unit)
	}
}

// TestParseUnitLinesFallsBackToTheNameRule: when ExecStart is unreadable the
// corroboration has no evidence, so the name rule and its deny-list must
// still apply rather than the unit being dropped or blindly accepted.
func TestParseUnitLinesFallsBackToTheNameRule(t *testing.T) {
	lines := "urnetwork.service loaded active running\n" +
		"urnetwork-update.service loaded active running\n"

	got := parseUnitLines(lines, nil,
		func(string) string { return "klets" },
		func(string) string { return "" }, // ExecStart unavailable
	)

	if len(got) != 1 || got[0].Unit != "urnetwork.service" {
		t.Fatalf("parseUnitLines = %+v, want only urnetwork.service (deny-list still excludes -update)", got)
	}
}
