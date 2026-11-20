package urnettools

import (
	"testing"
)

func TestStatusSummaryIPv6(t *testing.T) {
	s := &NodeSnapshot{
		Proxies: SnapshotProxies{
			Up: 1004,
			IPv6: &SnapshotProxyIPv6{
				Yes:        12,
				No:         988,
				Unknown:    4,
				NotProbed:  10,
				UDPYes:     10,
				UDPNo:      2,
				UDPUnknown: 0,
				Probes:     1004,
			},
		},
	}
	rows := makeStatusRows(Provider{Running: true}, s, ExtenderReading{}, nil)
	found := false
	for _, row := range rows {
		if row.Name == "IPv6" {
			found = true
			want := "IPv6 proxies: 12 yes, 988 no, 4 unknown, 10 not probed (udp: 10 yes, 2 no)"
			if row.Text != want {
				t.Fatalf("IPv6 row text got %q, want %q", row.Text, want)
			}
			break
		}
	}
	if !found {
		t.Fatal("IPv6 row missing in status rows")
	}
}

func TestStatusIPv6TextFormats(t *testing.T) {
	v6 := &SnapshotProxyIPv6{
		Yes:        5,
		No:         10,
		Unknown:    1,
		NotProbed:  0,
		UDPYes:     4,
		UDPNo:      1,
		UDPUnknown: 1,
	}
	got := statusIPv6Text(v6)
	want := "IPv6 proxies: 5 yes, 10 no, 1 unknown (udp: 4 yes, 1 no, 1 unknown)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStatusIPv6OmittedWhenNil(t *testing.T) {
	s := &NodeSnapshot{
		Proxies: SnapshotProxies{
			Up: 10,
		},
	}
	rows := makeStatusRows(Provider{Running: true}, s, ExtenderReading{}, nil)
	for _, row := range rows {
		if row.Name == "IPv6" {
			t.Fatalf("unexpected IPv6 row when nil: %+v", row)
		}
	}
}
