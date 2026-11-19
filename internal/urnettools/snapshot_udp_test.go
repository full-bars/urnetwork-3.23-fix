package urnettools

import (
	"strings"
	"testing"
)

func TestUDPLiveBlock(t *testing.T) {
	cases := []struct {
		name string
		udp  *SnapshotProxyUDP
		want []string
	}{
		{"missing", nil, nil},
		{"unchecked", &SnapshotProxyUDP{Unchecked: 4}, []string{"4 unchecked", "n/a of checked, 0% of online"}},
		{"empty", &SnapshotProxyUDP{}, []string{"n/a of checked, n/a of online"}},
		{"mixed", &SnapshotProxyUDP{OK: 3, NoRelay: 1, Mismatch: 1, Unknown: 1, Unchecked: 4}, []string{"3 ok", "1 no UDP", "1 mismatch", "1 unknown", "4 unchecked", "50% of checked, 30% of online"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := loadSnapshotFixture(t, "node_snapshot_v1.json")
			s.Proxies.UDP = c.udp
			for _, opts := range []liveOpts{{}, {ASCII: true}} {
				got := renderLiveBlock(s, opts, "")
				if c.udp == nil && strings.Contains(got, "udp") {
					t.Errorf("unexpected UDP row in %s", got)
				}
				for _, w := range c.want {
					if !strings.Contains(got, w) {
						t.Errorf("missing %q in %s", w, got)
					}
				}
			}
		})
	}
}
