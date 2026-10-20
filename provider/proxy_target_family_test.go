package main

// proxy_target_family_test.go — the provider side of the setting: it must be a
// real control key, validated, live, persisted, and re-applied at startup with
// the environment as its fallback.

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestProxyTargetFamilyIsALiveControlKey(t *testing.T) {
	if !controlKeys["proxy_target_family"] {
		t.Fatal("controlKeys has no proxy_target_family entry: set/clear would reject it and it could never persist")
	}
	if !liveEffectKeys["proxy_target_family"] {
		t.Fatal("liveEffectKeys has no proxy_target_family entry: changing it would claim a restart is needed")
	}
	if needsRestart("proxy_target_family") {
		t.Fatal("proxy_target_family reports that it needs a restart; it is read inside every dial")
	}
	if got := liveDefaults["proxy_target_family"]; got != "v4" {
		t.Fatalf("liveDefaults[proxy_target_family] = %q; want v4, so clearing the key restores today's behaviour", got)
	}
	if proxyTargetFamilyDefault != "v4" {
		t.Fatalf("proxyTargetFamilyDefault = %q; want v4", proxyTargetFamilyDefault)
	}
}

func TestValidateControlValueProxyTargetFamily(t *testing.T) {
	for _, good := range []string{"v4", "V4", "ipv4", " proxy ", "PROXY"} {
		if err := validateControlValue("proxy_target_family", good); err != nil {
			t.Errorf("validateControlValue(proxy_target_family, %q) = %v; want accepted", good, err)
		}
	}
	for _, bad := range []string{"v6", "ipv6", "1", "4", "on", "off", "domain", "proxy4"} {
		if err := validateControlValue("proxy_target_family", bad); err == nil {
			t.Errorf("validateControlValue(proxy_target_family, %q) must be rejected", bad)
		}
	}
}

func TestApplyProxyTargetFamily(t *testing.T) {
	previous := connect.SetProxyTargetFamily(connect.ProxyTargetFamilyV4)
	defer connect.SetProxyTargetFamily(previous)

	if err := applyProxyTargetFamily("proxy"); err != nil {
		t.Fatalf("apply proxy: %v", err)
	}
	if got := connect.ProxyTargetFamily(); got != connect.ProxyTargetFamilyProxy {
		t.Fatalf("mode after apply proxy = %v; want proxy", got)
	}
	if err := applyProxyTargetFamily("v4"); err != nil {
		t.Fatalf("apply v4: %v", err)
	}
	if got := connect.ProxyTargetFamily(); got != connect.ProxyTargetFamilyV4 {
		t.Fatalf("mode after apply v4 = %v; want v4", got)
	}
	// a value this build does not know must be refused AND must not move the
	// running mode: the caller reports the error and the dial path keeps the
	// last good value rather than silently changing behaviour
	if err := applyProxyTargetFamily("v6"); err == nil {
		t.Fatal("apply v6 must be refused")
	}
	if got := connect.ProxyTargetFamily(); got != connect.ProxyTargetFamilyV4 {
		t.Fatalf("a rejected value changed the running mode to %v", got)
	}
	// clearing the key lands on the same default
	if err := applyLiveDefault("proxy_target_family"); err != nil {
		t.Fatalf("applyLiveDefault: %v", err)
	}
	if got := connect.ProxyTargetFamily(); got != connect.ProxyTargetFamilyV4 {
		t.Fatalf("mode after clear = %v; want v4", got)
	}
}

func TestResolveProxyTargetFamilyPrecedence(t *testing.T) {
	// default with nothing set anywhere
	t.Setenv("URNETWORK_PROXY_TARGET_FAMILY", "")
	if got := resolveProxyTargetFamily(newControlState()); got != "v4" {
		t.Fatalf("default = %q; want v4", got)
	}

	// the environment is the startup default
	t.Setenv("URNETWORK_PROXY_TARGET_FAMILY", "proxy")
	if got := resolveProxyTargetFamily(newControlState()); got != "proxy" {
		t.Fatalf("environment value = %q; want proxy", got)
	}

	// a persisted key beats it, because the operator set it over the socket
	state := newControlState()
	if err := state.set("proxy_target_family", "v4"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := resolveProxyTargetFamily(state); got != "v4" {
		t.Fatalf("persisted value = %q; want v4", got)
	}

	// a blank persisted value falls through rather than pinning an invalid one
	blank := newControlState()
	blank.values["proxy_target_family"] = "   "
	if got := resolveProxyTargetFamily(blank); got != "proxy" {
		t.Fatalf("blank persisted value = %q; want the environment value proxy", got)
	}
}
