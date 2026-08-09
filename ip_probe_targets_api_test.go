package connect

import "testing"

// TestProbeTargetsAccessors: the exported accessors must expose the table
// and the deterministic rotation without changing upstream semantics.
func TestProbeTargetsAccessors(t *testing.T) {
	if n := len(ProbeHostNames()); n < 100 {
		t.Fatalf("expected a substantial health-host table, got %d entries", n)
	}
	if n := len(ProbeResolverIps()); n < 10 {
		t.Fatalf("expected a substantial resolver table, got %d entries", n)
	}
	if f := ProbePassFraction(); f <= 0.5 || f >= 1.0 {
		t.Fatalf("pass fraction must be in (0.5, 1.0), got %v", f)
	}
	// reputation-class sites must be absent (see ip_probe_targets.go header)
	for _, forbidden := range []string{"akamai", "reddit", "epic", "stackoverflow", "reuters", "etsy", "ecosia", "canva"} {
		for _, h := range ProbeHostNames() {
			if containsFold(h, forbidden) {
				t.Errorf("reputation-class host %q must not appear in the probe table", h)
			}
		}
	}
}

// TestSampleProbeTargets_Deterministic: the same seed yields the same block.
func TestSampleProbeTargets_Deterministic(t *testing.T) {
	h1, r1 := SampleProbeTargets(42, 8)
	h2, r2 := SampleProbeTargets(42, 8)
	if len(h1) != len(h2) {
		t.Fatalf("same seed must yield same block size: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("same seed must yield same block: %v vs %v", h1, h2)
		}
	}
	if r1 != r2 {
		t.Fatalf("same seed must yield same resolver: %v vs %v", r1, r2)
	}
}

// TestSampleProbeTargets_DisjointRotation: consecutive seeds return disjoint
// host blocks (block seed covers [seed*n, seed*n+n) mod table), so a proxy
// re-probed over a session walks the whole list instead of re-testing the
// same few sites.
func TestSampleProbeTargets_DisjointRotation(t *testing.T) {
	n := 8
	h0, _ := SampleProbeTargets(0, n)
	h1, _ := SampleProbeTargets(1, n)
	seen := map[string]bool{}
	for _, h := range h0 {
		seen[h] = true
	}
	overlap := 0
	for _, h := range h1 {
		if seen[h] {
			overlap++
		}
	}
	if overlap != 0 {
		t.Fatalf("consecutive seeds must be disjoint blocks, overlap=%d (%v vs %v)", overlap, h0, h1)
	}
}

// TestSampleProbeTargets_ClampsToTable: asking for more than the table holds
// returns the whole table, not an error or a short slice.
func TestSampleProbeTargets_ClampsToTable(t *testing.T) {
	hosts, _ := SampleProbeTargets(0, 100000)
	if len(hosts) != len(ProbeHostNames()) {
		t.Fatalf("expected clamp to table size %d, got %d", len(ProbeHostNames()), len(hosts))
	}
}

// TestSampleProbeTargets_ZeroWidth: n<=0 asks no hosts but may still pick a
// resolver, matching upstream behavior.
func TestSampleProbeTargets_ZeroWidth(t *testing.T) {
	hosts, _ := SampleProbeTargets(7, 0)
	if len(hosts) != 0 {
		t.Fatalf("expected zero hosts for n=0, got %v", hosts)
	}
}

func containsFold(s, sub string) bool {
	s = toLower(s)
	sub = toLower(sub)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
