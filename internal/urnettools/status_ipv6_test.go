package urnettools

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
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
	found, udpFound := false, false
	for _, row := range rows {
		if row.Name == "IPv6" {
			found = true
			want := "IPv6 proxies: 12 yes, 988 no, 4 unknown, 10 not probed"
			if row.Text != want {
				t.Fatalf("IPv6 row text got %q, want %q", row.Text, want)
			}
		}
		if row.Name == "IPv6 UDP" {
			udpFound = true
			want := "IPv6 UDP probes: 10 yes, 2 no"
			if row.Text != want {
				t.Fatalf("IPv6 UDP row text got %q, want %q", row.Text, want)
			}
		}
	}
	if !found || !udpFound {
		t.Fatalf("IPv6 rows missing in status rows: ipv6=%v udp=%v", found, udpFound)
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
	if got, want := statusIPv6Text(v6), "IPv6 proxies: 5 yes, 10 no, 1 unknown"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := statusIPv6UDPText(v6), "IPv6 UDP probes: 4 yes, 1 no, 1 unknown"; got != want {
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

// A narrow width must wrap the UDP summary at its separators. The whole
// summary is one segment otherwise and pushes the line past the width.
func TestIPv6RowWrapsTheUDPSummary(t *testing.T) {
	v6 := &SnapshotProxyIPv6{Yes: 1, No: 2, Unknown: 3, UDPYes: 10, UDPNo: 2, UDPUnknown: 1}
	row := ipv6row(v6)
	for _, ln := range wrapRow(row, 40) {
		if w := visibleWidth(ln); w > 40 {
			t.Fatalf("line %q is %d columns wide, want at most 40", ln, w)
		}
	}
	wide := wrapRow(row, 0)
	if len(wide) != 1 || !strings.Contains(wide[0], "(udp: 10 yes, 2 no, 1 unknown)") {
		t.Fatalf("unlimited width must keep the whole summary on one line: %q", wide)
	}
}

// renderStatusRows clips each row to 80 columns. With large counts the UDP
// unknown count must still be visible, which one combined row could not do.
func TestStatusIPv6RowsShowTheUdpUnknownCountAtWidth80(t *testing.T) {
	s := &NodeSnapshot{
		Proxies: SnapshotProxies{
			Up: 1300,
			IPv6: &SnapshotProxyIPv6{
				Yes: 1000, No: 100, Unknown: 150, NotProbed: 50,
				UDPYes: 1000, UDPNo: 100, UDPUnknown: 37,
			},
		},
	}
	rows := makeStatusRows(Provider{Running: true}, s, ExtenderReading{}, nil)
	var out bytes.Buffer
	if err := renderStatusRows(&out, rows, 80, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "37 unknown") {
		t.Fatalf("the UDP unknown count is not in the rendered rows:\n%s", out.String())
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if strings.Contains(line, "IPv6") && utf8.RuneCountInString(line) > 80 {
			t.Fatalf("a row is wider than 80 columns: %q", line)
		}
	}
}
