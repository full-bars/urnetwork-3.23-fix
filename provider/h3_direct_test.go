package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestH3EnabledParsesTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"off", false}, {"0", false}, {"false", false}, {"no", false}, {"garbage", false},
		{"on", true}, {"ON", true}, {" 1 ", true}, {"true", true}, {"Yes", true},
	} {
		t.Setenv("URNETWORK_H3", tc.value)
		if got := h3Enabled(); got != tc.want {
			t.Fatalf("URNETWORK_H3=%q: h3Enabled=%v, want %v", tc.value, got, tc.want)
		}
	}
}

// H3 is for the direct identity only. Even with the switch on, a proxied
// identity must never get it: runH3 opens a host UDP socket, which would send
// its QUIC from the host instead of its proxy.
func TestH3NeverEnabledForAProxiedIdentity(t *testing.T) {
	proxied := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1080"}

	t.Setenv("URNETWORK_H3", "on")
	if platformTransportSettingsFor(proxied, false).EnableH3 {
		t.Fatal("a proxied identity got H3")
	}
	if platformTransportSettingsFor(proxied, true).EnableH3 {
		t.Fatal("an identity with proxy settings got H3 even though flagged native")
	}
	if !platformTransportSettingsFor(nil, true).EnableH3 {
		t.Fatal("the direct identity did not get H3 with the switch on")
	}
	if platformTransportSettingsFor(nil, false).EnableH3 {
		t.Fatal("a non-native identity without proxy settings got H3")
	}

	t.Setenv("URNETWORK_H3", "")
	if platformTransportSettingsFor(nil, true).EnableH3 {
		t.Fatal("H3 must be off unless the operator opts in")
	}
}

// With the switch off the settings are exactly the engine defaults, so a box
// that does not opt in sees no change at all.
func TestPlatformTransportSettingsAreTheDefaultsWhenOff(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	got := platformTransportSettingsFor(nil, true)
	want := connect.DefaultPlatformTransportSettings()
	if got.EnableH3 != want.EnableH3 || got.H3Port != want.H3Port ||
		got.ReconnectTimeout != want.ReconnectTimeout || got.PingTimeout != want.PingTimeout ||
		got.AuthTimeout != want.AuthTimeout || got.TransportBufferSize != want.TransportBufferSize {
		t.Fatalf("settings differ from the engine defaults with H3 off: %+v vs %+v", got, want)
	}
}
