//go:build linux

package urnettools

import (
	"os"
	"os/exec"
	"os/user"
	"testing"
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

	providers := discoverProcesses()

	var found *Provider
	for i := range providers {
		if providers[i].PID == cmd.Process.Pid {
			found = &providers[i]
			break
		}
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

	p, narrowed, err := selectTargetOrSoleAccessible(providers, Target{})
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
// and must never pick a non-running provider (Sonnet backlog #1a).
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

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{})
	if err == nil {
		t.Fatal("expected an error when the caller's sole own provider is stopped (no running sole candidate)")
	}
	if narrowed {
		t.Error("must not auto-narrow to a STOPPED provider")
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

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{})
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

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{})
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
	got := parseUnitLines(text, nil, func(string) string { return "urnet" })
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
	got := parseUnitLines(text, running, func(string) string { return "urnet" })
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

	p, narrowed, err := selectTargetOrSoleAccessible(providers, Target{Unit: "urnetwork-alpha.service"})
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
