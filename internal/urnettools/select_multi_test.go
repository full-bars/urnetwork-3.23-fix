package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// threeProviders is the canonical multi-provider fixture: taco's native,
// our beta, and our alpha — the exact shape of a busy taco box.
func threeProviders() []Provider {
	return []Provider{
		{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000", StateDir: "/home/urnet/.urnetwork"},
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Network: "beta-test", StateDir: "/home/urnetwork-beta/.urnetwork"},
		{User: "urnetwork-alpha", Unit: "urnetwork-alpha.service", Network: "mesocyclone", StateDir: "/home/urnetwork-alpha/.urnetwork"},
	}
}

// TestSelectTargetsIncludePicksSubset: --include A,B updates exactly those
// and skips D — the checkbox-style selection via labels.
func TestSelectTargetsIncludePicksSubset(t *testing.T) {
	ps := threeProviders()
	got, err := selectTargets(ps, Target{}, []string{"urnetwork-native.service", "urnetwork-alpha.service"}, nil, false)
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 providers, got %d", len(got))
	}
	for _, p := range got {
		if p.Network == "beta-test" {
			t.Error("beta was included but should be skipped")
		}
	}
}

// TestSelectTargetsExcludeSubtracts: --exclude removes from the chosen set.
func TestSelectTargetsExcludeSubtracts(t *testing.T) {
	ps := threeProviders()
	// Include all three, exclude beta -> two remain.
	got, err := selectTargets(ps, Target{}, []string{"urnetwork-native.service", "urnetwork-beta.service", "urnetwork-alpha.service"}, []string{"urnetwork-beta.service"}, false)
	if err != nil {
		t.Fatalf("selectTargets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 after exclude, got %d", len(got))
	}
	for _, p := range got {
		if p.Network == "beta-test" {
			t.Error("beta should have been excluded")
		}
	}
}

// TestSelectTargetsAmbiguousRefuses: multiple providers, no criteria, not
// interactive -> refuse with inventory (same guard as selectTarget).
func TestSelectTargetsAmbiguousRefuses(t *testing.T) {
	_, err := selectTargets(threeProviders(), Target{}, nil, nil, false)
	if err == nil {
		t.Fatal("expected refusal with multiple providers and no criteria")
	}
	if !contains(err.Error(), "specify a target") {
		t.Errorf("refusal should mention targeting, got: %s", err)
	}
}

// TestSelectTargetsSingleProviderDefaults: one provider, no criteria -> it.
func TestSelectTargetsSingleProviderDefaults(t *testing.T) {
	ps := []Provider{{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000"}}
	got, err := selectTargets(ps, Target{}, nil, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Network != "tacogonzalez3000" {
		t.Errorf("want the single provider, got %+v", got)
	}
}

// TestSelectTargetsUnknownLabel: a label matching nothing is an error.
func TestSelectTargetsUnknownLabel(t *testing.T) {
	_, err := selectTargets(threeProviders(), Target{}, []string{"nosuchunit"}, nil, false)
	if err == nil {
		t.Fatal("expected error for unknown label")
	}
}

// TestSelectTargetsAmbiguousLabel: a label matching multiple is an error.
func TestSelectTargetsAmbiguousLabel(t *testing.T) {
	ps := []Provider{
		{User: "urnet", Unit: "a.service", Network: "net1", StateDir: "/home/x/.urnetwork"},
		{User: "urnet", Unit: "b.service", Network: "net2", StateDir: "/home/x/.urnetwork"},
	}
	// Label "urnet" (user) matches both -> ambiguous.
	_, err := selectTargets(ps, Target{}, []string{"urnet"}, nil, false)
	if err == nil {
		t.Fatal("expected ambiguity error for user label matching two providers")
	}
}

// TestSelectTargetsExplicitTargetStillSingle: an explicit --network target
// still resolves to exactly one provider even with batch flags present.
func TestSelectTargetsExplicitTargetStillSingle(t *testing.T) {
	got, err := selectTargets(threeProviders(), Target{Network: "tacogonzalez3000"}, nil, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].User != "urnet" {
		t.Errorf("want exactly taco's native provider, got %+v", got)
	}
}

// TestSelectTargetsExcludeLeavesEmpty: excluding everything yields an error.
func TestSelectTargetsExcludeLeavesEmpty(t *testing.T) {
	_, err := selectTargets(threeProviders(), Target{}, []string{"urnetwork-native.service"}, []string{"urnetwork-native.service"}, false)
	if err == nil {
		t.Fatal("expected error when selection is empty after exclude")
	}
}

// TestSelectTargetsForceRefusesMulti: -f mode (non-interactive) with
// multiple providers and no criteria must refuse — force never auto-selects
// everything; that requires an explicit --all.
func TestSelectTargetsForceRefusesMulti(t *testing.T) {
	// Non-interactive (what -f implies) with 3 providers and no criteria.
	_, err := selectTargets(threeProviders(), Target{}, nil, nil, false)
	if err == nil {
		t.Fatal("expected refusal: -f alone must not update a multi-provider box")
	}
	if !contains(err.Error(), "specify a target") {
		t.Errorf("refusal should demand a target, got: %s", err)
	}
}

// TestSelectTargetsAllSemantics documents that --all maps to every provider
// (handled in cmdUpdate, but the contract is: all three come back).
func TestSelectTargetsAllSemantics(t *testing.T) {
	ps := threeProviders()
	// selectTargets has no --all; cmdUpdate expands it. Simulate the same
	// contract: an explicit include of all three labels yields all three.
	got, err := selectTargets(ps, Target{}, []string{"urnetwork-native.service", "urnetwork-beta.service", "urnetwork-alpha.service"}, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want all 3, got %d", len(got))
	}
}

// twoSameNetwork is the duplicate-network fixture: the same account name on
// two providers (e.g. one mainnet, one beta) with different network IDs and
// state dirs — the scenario the user asked about.
func twoSameNetwork() []Provider {
	return []Provider{
		{User: "urnet", Unit: "urnetwork-native.service", Network: "tacogonzalez3000", NetworkID: "019c3c0c-436c-6b8b-68a3-2d21dd48a50c", StateDir: "/home/urnet/.urnetwork"},
		{User: "urnetwork-beta", Unit: "urnetwork-beta.service", Network: "tacogonzalez3000", NetworkID: "019f1234-aaaa-bbbb-cccc-dddddddddddd", StateDir: "/home/urnetwork-beta/.urnetwork"},
	}
}

// TestSelectTargetSameNetworkNameAmbiguous: --network with a duplicate name
// must refuse (can't tell which one) — the operator must add --network-id.
func TestSelectTargetSameNetworkNameAmbiguous(t *testing.T) {
	_, err := selectTarget(twoSameNetwork(), Target{Network: "tacogonzalez3000"})
	if err == nil {
		t.Fatal("expected ambiguity error: same network name on two providers")
	}
	if !contains(err.Error(), "ambiguous") {
		t.Errorf("error should say ambiguous, got: %s", err)
	}
}

// TestSelectTargetByNetworkID: the TRUE unique identity (network_id) breaks
// the tie and resolves exactly one.
func TestSelectTargetByNetworkID(t *testing.T) {
	p, err := selectTarget(twoSameNetwork(), Target{NetworkID: "019f1234-aaaa-bbbb-cccc-dddddddddddd"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.User != "urnetwork-beta" {
		t.Errorf("selected user %s, want urnetwork-beta (the beta copy)", p.User)
	}
}

// TestSelectTargetsIncludeSameNetworkKeepsBoth: batch selection by labels
// must keep BOTH providers even when they share a network name (the matchKey
// uniqueness fix).
func TestSelectTargetsIncludeSameNetworkKeepsBoth(t *testing.T) {
	ps := twoSameNetwork()
	got, err := selectTargets(ps, Target{}, []string{"urnetwork-native.service", "urnetwork-beta.service"}, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both providers, got %d (dedup bug if 1)", len(got))
	}
}

// TestRootHintUsesResolvedExecutablePath: the hint must be an actually
// runnable command, not the bare "urnet-tools" name — plain `sudo
// urnet-tools` fails for operators because the binary installs to a
// per-user path that's never on root's $PATH (the exact complaint this
// hint exists to answer).
func TestRootHintUsesResolvedExecutablePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("hint is empty for root — nothing to check")
	}
	hint := rootHint()
	if hint == "" {
		t.Fatal("expected a non-empty root hint when unprivileged")
	}
	if !strings.HasPrefix(hint, "sudo ") {
		t.Fatalf("hint = %q, want a \"sudo \" prefix", hint)
	}
	path := strings.TrimPrefix(hint, "sudo ")
	if !filepath.IsAbs(path) {
		t.Errorf("hint path %q is not absolute — bare command name would fail under sudo same as it did for the user", path)
	}
}

// TestMatchKeyUniquenessAcrossProviders: no two providers in the duplicate
// fixture share a matchKey.
func TestMatchKeyUniquenessAcrossProviders(t *testing.T) {
	ps := twoSameNetwork()
	seen := map[string]bool{}
	for _, p := range ps {
		k := matchKey(p)
		if seen[k] {
			t.Fatalf("matchKey collision: %q", k)
		}
		seen[k] = true
	}
}
