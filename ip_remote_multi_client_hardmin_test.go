package connect

import "testing"

func hardMinTestClient(family IpFamily) *multiClientChannel {
	return &multiClientChannel{
		args: &multiClientChannelArgs{
			DestinationStats: DestinationStats{IpFamily: family},
		},
	}
}

// The resize pass judges the window minimum with hardMinimumCount: a window
// of only v6-only exits counts as empty, a v6-only exit beside a v4-capable
// one keeps its slot, and a fixed destination counts whatever it holds.
func TestHardMinimumCount(t *testing.T) {
	v6 := hardMinTestClient(IpFamilyV6Only)
	v4 := hardMinTestClient(IpFamilyV4Only)
	dual := hardMinTestClient(IpFamilyDualstack)

	cases := []struct {
		name    string
		clients []*multiClientChannel
		fixed   bool
		want    int
	}{
		{"empty", nil, false, 0},
		{"v6 only window counts as empty", []*multiClientChannel{v6, v6}, false, 0},
		{"v6 beside v4 keeps its slot", []*multiClientChannel{v6, v4}, false, 2},
		{"v6 beside dualstack keeps its slot", []*multiClientChannel{v6, dual}, false, 2},
		{"fixed destination counts a v6 exit", []*multiClientChannel{v6}, true, 1},
	}
	for _, c := range cases {
		if got := hardMinimumCount(c.clients, c.fixed); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
