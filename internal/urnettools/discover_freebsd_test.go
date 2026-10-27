//go:build freebsd

package urnettools

import "testing"

// The installer writes the provider to a root-owned tree that is not derived
// from a home directory. A candidate list built only from $HOME cannot find a
// stopped provider installed there, which is every install the current script
// makes.
func TestBSDProviderCandidatesPrefersTheRootOwnedTree(t *testing.T) {
	got := bsdProviderCandidates("/home/urnet")
	want := []string{
		"/usr/local/libexec/urnetwork-provider/bin/urnetwork",
		"/home/urnet/.local/share/urnetwork-provider/bin/urnetwork",
		"/home/urnet/.urnetwork/urnetwork",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBSDProviderCandidatesKeepTheRootTreeWithoutAHome(t *testing.T) {
	got := bsdProviderCandidates("")
	if len(got) != 1 || got[0] != "/usr/local/libexec/urnetwork-provider/bin/urnetwork" {
		t.Fatalf("candidates for an unknown home = %v, want only the root-owned tree", got)
	}
}
