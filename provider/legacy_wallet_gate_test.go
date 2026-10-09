package main

import (
	"strings"
	"testing"
)

func TestLegacyNetworkWalletGateRefusesWithoutOptIn(t *testing.T) {
	err := legacyNetworkWalletGate(false, "provider wallet set")
	if err == nil {
		t.Fatal("the unsigned wallet request must be refused without --legacy-network-wallet")
	}
	for _, want := range []string{"provider wallet set", "--legacy-network-wallet", "app or web account"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say %q, got %q", want, err.Error())
		}
	}
	// the message must not state server policy as fact or name a command this
	// binary does not have
	for _, bad := range []string{"no longer accepts", "hotkey set"} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("the refusal must not say %q, got %q", bad, err.Error())
		}
	}
	if err := legacyNetworkWalletGate(true, "provider wallet set"); err != nil {
		t.Fatalf("an explicit opt-in must pass: %v", err)
	}
}
