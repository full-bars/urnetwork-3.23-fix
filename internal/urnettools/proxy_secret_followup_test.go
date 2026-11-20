package urnettools

import (
	"strings"
	"testing"
)

func TestProxyIDLabelHidesTheUser(t *testing.T) {
	key := "198.51.100.30:443\x1fcanary-user-7731"
	got := proxyIDLabel(key)
	if strings.Contains(got, "canary-user-7731") || strings.Contains(got, "\x1f") {
		t.Fatalf("label leaks the user: %q", got)
	}
	if got != "198.51.100.30:443 (user ***)" {
		t.Fatalf("label = %q", got)
	}
	if proxyIDLabel("direct") != "(direct)" {
		t.Fatal("direct label")
	}
	if got := proxyIDLabel("[canary-user-7731:canary-pass-9042]:1080"); strings.Contains(got, "canary") {
		t.Fatalf("bracketed credentials leak: %q", got)
	}
}

func TestProxyDisplayAddressRefusesColonUserinfo(t *testing.T) {
	for _, in := range []string{"canary-user-7731:canary-pass-9042:example.com:1080", "[canary-user-7731:canary-pass-9042]:1080"} {
		if got := proxyDisplayAddress(in); got != "?" {
			t.Errorf("proxyDisplayAddress(%q) = %q", in, got)
		}
	}
	if got := proxyDisplayAddress("example.com:1080"); got != "example.com:1080" {
		t.Errorf("plain: %q", got)
	}
}
