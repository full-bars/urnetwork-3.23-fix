package nicstat

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseLinuxCounters(t *testing.T) {
	fixture := "Inter-|   Receive | Transmit\n face | bytes packets errs drop fifo frame compressed multicast | bytes packets errs drop fifo colls carrier compressed\n" +
		" lo: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n" +
		" bridge0: 123 0 0 0 0 0 0 0 456 0 0 0 0 0 0 0\n" +
		" veth123: 99 0 0 0 0 0 0 0 88 0 0 0 0 0 0 0\n" +
		" tun0: 7 0 0 0 0 0 0 0 8 0 0 0 0 0 0 0\n"
	got, err := parseLinuxCounters(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[1].Name != "bridge0" || got[1].RxBytes != 123 || got[1].TxBytes != 456 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
	for _, bad := range []string{"", "header\nheader\ninvalid", "header\nheader\nx: bad 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"} {
		if _, err := parseLinuxCounters(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted malformed %q", bad)
		}
	}
}

func TestParseLinuxRoute(t *testing.T) {
	fixture := "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n" +
		"br0 00000000 01000000 0001 0 0 99 00000000 0 0 0\n" +
		"veth0 00000000 01000000 0000 0 0 1 00000000 0 0 0\n" +
		"tun0 00000000 01000000 0003 0 0 5 00000000 0 0 0\n" +
		"lo 00000000 00000000 0001 0 0 0 FFFFFFFF 0 0 0\n"
	got, err := parseLinuxRoute(strings.NewReader(fixture))
	if err != nil || got != "tun0" {
		t.Fatalf("got %q, %v", got, err)
	}
	_, err = parseLinuxRoute(strings.NewReader("header\n"))
	if !errors.Is(err, ErrNoDefaultRoute) {
		t.Fatalf("expected no route, got %v", err)
	}
}

// TestSmoke is opt-in because it displays local interface names.
func TestSmoke(t *testing.T) {
	if os.Getenv("NICSTAT_SMOKE") != "1" {
		t.Skip("set NICSTAT_SMOKE=1 to display interface names")
	}
	all, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		t.Logf("interface: %s", c.Name)
	}
	name, err := DefaultRoute()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default route: %s", name)
	for _, c := range all {
		if c.Name == name {
			return
		}
	}
	t.Fatalf("default route interface not in snapshot: %s", name)
}
