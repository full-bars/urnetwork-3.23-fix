package main

import "testing"

func TestRedactedProxyAddressRefusesBracketedCredentials(t *testing.T) {
	const bad = "<unparsable proxy address>"
	for _, in := range []string{
		"[canary-user-7731:canary-pass-9042]:1080",
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"example.com:0",
		"[fe80::1%eth0]:1080",
	} {
		if got := redactedProxyAddress(in); got != bad {
			t.Errorf("redactedProxyAddress(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"example.com:1080", "[2001:db8::1]:1080", "direct"} {
		if got := redactedProxyAddress(in); got != in {
			t.Errorf("redactedProxyAddress(%q) = %q", in, got)
		}
	}
}

func TestProxyCellSafeAddressRefusesColonUserinfo(t *testing.T) {
	for _, in := range []string{
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"[canary-user-7731:canary-pass-9042]:1080",
	} {
		if got := proxyCellSafeAddress(3, in); got != "?" {
			t.Errorf("proxyCellSafeAddress(%q) = %q", in, got)
		}
	}
	if got := proxyCellSafeAddress(3, "[2001:db8::1]:1080"); got != "[2001:db8::1]:1080" {
		t.Errorf("an IPv6 proxy address must stay readable, got %q", got)
	}
	if got := proxyCellSafeAddress(0, "x"); got != "direct (this host)" {
		t.Errorf("direct slot: %q", got)
	}
}
