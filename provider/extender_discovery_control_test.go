package main

import (
	"context"
	"strings"
	"testing"
)

func TestExtenderDiscoveryControlQueuedAndLive(t *testing.T) {
	home := withTempHome(t)
	previous := globalControlState
	state := newControlState()
	globalControlState = state
	globalExtenderRole.Store(nil)
	t.Cleanup(func() { globalControlState = previous; globalExtenderRole.Store(nil) })
	if !controlKeys["extender_discovery"] || !liveEffectKeys["extender_discovery"] || liveDefaults["extender_discovery"] != "on" {
		t.Fatal("control key not registered")
	}
	for _, value := range []string{"off", "on", "feed-only", "ON"} {
		if err := validateControlValue("extender_discovery", value); err != nil {
			t.Fatal(err)
		}
	}
	if validateControlValue("extender_discovery", "invalid") == nil {
		t.Fatal("accepted invalid mode")
	}
	response := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_discovery", Value: "on"})
	// With no extender role the value is saved and applies when the role
	// starts, so the command is complete and nothing waits for a restart.
	if !response.OK || response.NeedsRestart {
		t.Fatalf("setting with no role: %+v", response)
	}
	if value, found := state.get("extender_discovery"); !found || value != "on" {
		t.Fatalf("setting was not persisted: %q %v", value, found)
	}
	writePendingOverrides(t, home, `[{"op":"set","key":"extender_discovery","value":"feed-only"}]`)
	mergePendingOverrides(state)
	loaded, err := loadControlState()
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := loaded.get("extender_discovery"); !ok || v != "feed-only" {
		t.Fatalf("persisted: %q %v", v, ok)
	}
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	t.Cleanup(func() { m.stop("test complete") })
	if got := role.discoverySnapshot(); got.Requested != "feed-only" || got.State != "waiting" {
		t.Fatalf("queued mode: %+v", got)
	}
	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_discovery", Value: "on"})
	if !res.OK || res.NeedsRestart || role.discoverySnapshot().Requested != "on" {
		t.Fatalf("live set: %+v", res)
	}
	res = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "extender_discovery"})
	if !res.OK || role.discoverySnapshot().Requested != "on" {
		t.Fatalf("clear: %+v %+v", res, role.discoverySnapshot())
	}
}

func TestExtenderDiscoveryMetrics(t *testing.T) {
	globalExtenderRole.Store(nil)
	text := renderExtenderRoleMetrics(extenderRoleSnapshot{})
	for _, want := range []string{`urnet_extender_discovery_state{state="off"} 1`, `urnet_extender_discovery_state{state="failed"} 0`, "urnet_extender_discovery_peers 0", "urnet_extender_discovery_feed_subscribers 0", "urnet_extender_tcp_retry_total 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestExtenderDiscoveryManagerDefaultsAndSavedModes(t *testing.T) {
	for _, tc := range []struct{ name, saved, want string }{
		{"default", "", "on"}, {"explicit-off", "off", "off"}, {"explicit-feed-only", "feed-only", "feed-only"}, {"invalid-saved", "invalid", "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			previous := globalControlState
			globalControlState = newControlState()
			globalExtenderRole.Store(nil)
			t.Cleanup(func() { globalControlState = previous; globalExtenderRole.Store(nil) })
			if tc.saved != "" {
				if err := globalControlState.set("extender_discovery", tc.saved); err != nil {
					t.Fatal(err)
				}
			}
			m := newExtenderManager()
			m.environ = func(string) string { return "" }
			m.directEnabled = func() bool { return true }
			m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.ctx = ctx
			m.Reconcile("test")
			role := m.Role()
			if role == nil {
				t.Fatal("extender role did not start")
			}
			t.Cleanup(func() { m.stop("test complete") })
			if got := role.discoverySnapshot().Requested; got != tc.want {
				t.Fatalf("requested %q, want %q", got, tc.want)
			}
		})
	}
}
