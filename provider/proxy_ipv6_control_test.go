package main

import (
	"testing"
)

func TestProxyIPv6ProbeControl(t *testing.T) {
	// the defaults are asserted, so an inherited override must not leak in
	t.Setenv("URNETWORK_PROXY_IPV6_PROBE", "")
	state := newControlState()
	if !proxyIPv6ProbeEnabled(state) || needsRestart("proxy_ipv6_probe") {
		t.Fatal("default must be enabled and live")
	}
	for _, value := range []string{"off", "on", "OFF", "ON"} {
		if err := validateControlValue("proxy_ipv6_probe", value); err != nil {
			t.Fatalf("validation failed for %q: %v", value, err)
		}
		if err := state.set("proxy_ipv6_probe", value); err != nil {
			t.Fatalf("set failed for %q: %v", value, err)
		}
		want := value == "on" || value == "ON"
		if got := proxyIPv6ProbeEnabled(state); got != want {
			t.Fatalf("%s: got %v, want %v", value, got, want)
		}
	}
	if validateControlValue("proxy_ipv6_probe", "invalid") == nil {
		t.Fatal("invalid value accepted")
	}
	// clear from an explicit off, so a clear that kept the override fails here
	if err := state.set("proxy_ipv6_probe", "off"); err != nil || proxyIPv6ProbeEnabled(state) {
		t.Fatal("off must disable the probe before the clear")
	}
	if err := state.clear("proxy_ipv6_probe"); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if _, found := state.get("proxy_ipv6_probe"); found {
		t.Fatal("clear must remove the override")
	}
	if !proxyIPv6ProbeEnabled(state) {
		t.Fatal("clear must restore default (on)")
	}
}

func TestProxyIPv6ProbeEnvDefault(t *testing.T) {
	for _, envVal := range []string{"off", "0", "false", "no"} {
		t.Setenv("URNETWORK_PROXY_IPV6_PROBE", envVal)
		state := newControlState()
		if proxyIPv6ProbeEnabled(state) {
			t.Fatalf("expected env %q to disable ipv6 probe", envVal)
		}
	}
	t.Setenv("URNETWORK_PROXY_IPV6_PROBE", "on")
	state := newControlState()
	if !proxyIPv6ProbeEnabled(state) {
		t.Fatal("expected env on to enable ipv6 probe")
	}
	t.Setenv("URNETWORK_PROXY_IPV6_PROBE", "")
	state = newControlState()
	if !proxyIPv6ProbeEnabled(state) {
		t.Fatal("expected unset env to default to on")
	}
}

func TestProxyIPv6ProbeLiveSideEffect(t *testing.T) {
	old, had := globalControlState.get("proxy_ipv6_probe")
	t.Cleanup(func() {
		if had {
			_ = globalControlState.set("proxy_ipv6_probe", old)
		} else {
			_ = globalControlState.clear("proxy_ipv6_probe")
		}
	})

	if err := globalControlState.set("proxy_ipv6_probe", "off"); err != nil {
		t.Fatal(err)
	}
	if err := applyLiveSideEffect("proxy_ipv6_probe", "off"); err != nil {
		t.Fatalf("apply live off failed: %v", err)
	}
	if proxyIPv6ProbeEnabled(globalControlState) {
		t.Fatal("expected globalControlState to be disabled after set off")
	}

	if err := globalControlState.set("proxy_ipv6_probe", "on"); err != nil {
		t.Fatal(err)
	}
	if err := applyLiveSideEffect("proxy_ipv6_probe", "on"); err != nil {
		t.Fatalf("apply live on failed: %v", err)
	}
	if !proxyIPv6ProbeEnabled(globalControlState) {
		t.Fatal("expected globalControlState to be enabled after set on")
	}
}
