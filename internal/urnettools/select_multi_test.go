package urnettools

import "testing"

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
