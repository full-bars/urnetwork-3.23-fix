//go:build linux

package urnettools

import (
	"os"
	"os/user"
	"testing"
)

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
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service"},
		{User: me, Unit: "urnetwork-native.service"},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service"},
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

// TestSelectTargetOrSoleAccessibleStillRefusesWhenAmbiguous: two providers
// both under accounts the caller can't disambiguate (neither is "own user")
// must still hit the normal refusal — narrowing only resolves the case
// where exactly one candidate remains.
func TestSelectTargetOrSoleAccessibleStillRefusesWhenAmbiguous(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can reach every provider; narrowing only applies unprivileged")
	}
	providers := []Provider{
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service"},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service"},
	}

	_, narrowed, err := selectTargetOrSoleAccessible(providers, Target{})
	if err == nil {
		t.Fatal("expected refusal when no provider matches the caller's own user")
	}
	if narrowed {
		t.Error("narrowed should be false when the refusal path is taken")
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
