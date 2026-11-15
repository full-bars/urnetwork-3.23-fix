//go:build linux

package urnettools

import "testing"

func TestProviderUnitInstances(t *testing.T) {
	for _, tc := range []struct {
		unit string
		want bool
	}{
		{"urnetwork.service", true},
		{"urnetwork@ipv6.service", true},
		{"urnetwork-native.service", true},
		{"urnetwork@.service", false},
		{"urnetwork@ipv6.timer", false},
		{"other@ipv6.service", false},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			if got := isProviderUnit(tc.unit); got != tc.want {
				t.Fatalf("isProviderUnit(%q) = %t, want %t", tc.unit, got, tc.want)
			}
		})
	}
}

func TestParseUnitLinesInstanceExecStart(t *testing.T) {
	listing := "urnetwork@ipv6.service loaded active running\nurnetwork@.service enabled\nurnetwork-bad@x.service loaded active running\n"
	got := parseUnitLines(listing, nil, func(string) string { return "" }, func(unit string) string {
		if unit == "urnetwork-bad@x.service" {
			return "/usr/bin/not-provider"
		}
		return "/usr/bin/urnetwork"
	})
	if len(got) != 1 || got[0].Unit != "urnetwork@ipv6.service" {
		t.Fatalf("unexpected providers: %+v", got)
	}
}
