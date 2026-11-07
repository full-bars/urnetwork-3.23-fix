package main

import (
	"strings"
	"testing"
)

// The extender role is on by default. An operator turns it off, and keeps it
// off, with `urnet-tools extender off`: a control key that persists, applies
// live and is validated, so no unit file or drop-in is ever needed.
func TestExtenderControlKeyIsRegistered(t *testing.T) {
	if !controlKeys[extenderControlKey] {
		t.Fatal("controlKeys has no extender entry: `extender off` would not persist across restarts")
	}
	if !liveEffectKeys[extenderControlKey] {
		t.Fatal("liveEffectKeys has no extender entry: the switch would need a restart")
	}
	if got := liveDefaults[extenderControlKey]; got != "on" {
		t.Fatalf("liveDefaults[extender] = %q, want on (clearing the key returns to the default)", got)
	}
}

func TestExtenderControlValueIsValidated(t *testing.T) {
	withTempHome(t)
	state := newControlState()
	for _, ok := range []string{"on", "off", "ON", "Off"} {
		resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: extenderControlKey, Value: ok})
		if !resp.OK {
			t.Errorf("set extender %q rejected: %s", ok, resp.Error)
		}
	}
	for _, bad := range []string{"", "maybe", "1", "auto"} {
		resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: extenderControlKey, Value: bad})
		if resp.OK || !strings.Contains(resp.Error, "extender") {
			t.Errorf("set extender %q must be rejected with a clear error, got ok=%v err=%q", bad, resp.OK, resp.Error)
		}
	}
}

func TestExtenderSettingResolution(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == extenderEnv {
				return value
			}
			return ""
		}
	}
	tests := []struct {
		name    string
		control string // "" means the control key is unset
		env     string
		wantOn  bool
		wantSrc string
	}{
		{"default is on", "", "", true, "default"},
		{"env off", "", "off", false, "URNETWORK_EXTENDER"},
		{"env 0", "", "0", false, "URNETWORK_EXTENDER"},
		{"env on", "", "on", true, "URNETWORK_EXTENDER"},
		{"unrecognised env keeps the default", "", "banana", true, "default"},
		{"control off beats env on", "off", "on", false, "control socket"},
		{"control on beats env off", "on", "off", true, "control socket"},
		{"control off with no env", "off", "", false, "control socket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			prev := globalControlState
			state := newControlState()
			globalControlState = state
			t.Cleanup(func() { globalControlState = prev })
			if tc.control != "" {
				if err := state.set(extenderControlKey, tc.control); err != nil {
					t.Fatal(err)
				}
			}
			gotOn, gotSrc := extenderSetting(env(tc.env))
			if gotOn != tc.wantOn {
				t.Errorf("on = %v, want %v", gotOn, tc.wantOn)
			}
			if !strings.Contains(gotSrc, tc.wantSrc) {
				t.Errorf("source = %q, want it to mention %q", gotSrc, tc.wantSrc)
			}
		})
	}
}

// `urnet-tools extender status` asks the control socket, which must answer with
// the whole picture whether the role is running or off.
func TestControlSocketExtenderStatusAnswersWhenOff(t *testing.T) {
	withTempHome(t)
	prev := globalControlState
	state := newControlState()
	globalControlState = state
	t.Cleanup(func() { globalControlState = prev })
	if err := state.set(extenderControlKey, "off"); err != nil {
		t.Fatal(err)
	}

	resp := handleControlRequest(state, controlRequest{Cmd: "extender"})
	if !resp.OK || resp.Extender == nil {
		t.Fatalf("extender status must answer, got ok=%v extender=%v err=%q", resp.OK, resp.Extender, resp.Error)
	}
	if !resp.Extender.Known || resp.Extender.Running || resp.Extender.Setting != "off" {
		t.Fatalf("reading = %+v, want known, not running, setting off", resp.Extender)
	}
	if !strings.Contains(resp.Extender.SettingSource, "control socket") {
		t.Fatalf("setting source %q does not name the control socket", resp.Extender.SettingSource)
	}
}
