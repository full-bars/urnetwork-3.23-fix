package main

import (
	connect "github.com/urnetwork/connect"
	"testing"
)

func TestP2PSourceControlKeys(t *testing.T) {
	for _, tc := range []struct {
		key  string
		good []string
		bad  string
	}{
		{"p2p_source_binding", []string{"off", "count", "drop"}, "on"},
		{"p2p_forged_source_drop", []string{"on", "off"}, "count"},
	} {
		if needsRestart(tc.key) {
			t.Errorf("%s must be live", tc.key)
		}
		for _, v := range tc.good {
			if err := validateControlValue(tc.key, v); err != nil {
				t.Errorf("%s=%s: %v", tc.key, v, err)
			}
		}
		if err := validateControlValue(tc.key, tc.bad); err == nil {
			t.Errorf("%s=%s accepted", tc.key, tc.bad)
		}
	}
}

func TestP2PSourceControlRestore(t *testing.T) {
	t.Cleanup(func() { _ = connect.SetP2PSourceBinding("count"); connect.SetP2PForgedSourceDrop(true) })
	state := newControlState()
	if err := state.set("p2p_source_binding", "drop"); err != nil {
		t.Fatal(err)
	}
	if err := state.set("p2p_forged_source_drop", "off"); err != nil {
		t.Fatal(err)
	}
	applyPersistedRuntimeTuning(state)
	if got := connect.P2PSourceBinding(); got != "drop" {
		t.Fatalf("mode=%s", got)
	}
	if connect.P2PForgedSourceDropEnabled() {
		t.Fatal("switch still on")
	}
	if err := applyLiveDefault("p2p_source_binding"); err != nil {
		t.Fatal(err)
	}
	if err := applyLiveDefault("p2p_forged_source_drop"); err != nil {
		t.Fatal(err)
	}
	if connect.P2PSourceBinding() != "count" || !connect.P2PForgedSourceDropEnabled() {
		t.Fatal("defaults not restored")
	}
}
