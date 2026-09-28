package main

import (
	"strings"
	"testing"
)

// The OOM cap needs a kill switch an operator can flip without editing the
// systemd unit: `urnet-tools set oom-cap off`. It is a control key that
// persists, applies live (effectiveTrimCap reads the mode on every reload) and
// is validated.
func TestOOMCapControlKeyIsRegistered(t *testing.T) {
	if !controlKeys["oom_cap"] {
		t.Fatal("controlKeys has no oom_cap entry: the kill switch would never persist")
	}
	if !liveEffectKeys["oom_cap"] {
		t.Fatal("liveEffectKeys has no oom_cap entry: the switch would need a restart")
	}
	if got := liveDefaults["oom_cap"]; got != "shadow" {
		t.Fatalf("liveDefaults[oom_cap] = %q, want shadow (clearing the key must return to the safe default)", got)
	}
}

func TestOOMCapControlValueIsValidated(t *testing.T) {
	state := newControlState()
	for _, ok := range []string{"on", "off", "shadow", "ON", "Shadow"} {
		resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "oom_cap", Value: ok})
		if !resp.OK {
			t.Errorf("set oom_cap %q rejected: %s", ok, resp.Error)
		}
	}
	for _, bad := range []string{"", "maybe", "1", "enforce"} {
		resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "oom_cap", Value: bad})
		if resp.OK || !strings.Contains(resp.Error, "oom_cap") {
			t.Errorf("set oom_cap %q must be rejected with a clear error, got ok=%v err=%q", bad, resp.OK, resp.Error)
		}
	}
}

// Precedence: ANY source saying off wins (a kill switch must always work, even
// against an env var that says on); otherwise ANY on wins; otherwise shadow.
func TestOOMCapModePrecedence(t *testing.T) {
	cases := []struct {
		env, control string
		want         oomCapModeKind
	}{
		{"", "", oomCapShadow},
		{"on", "", oomCapOn},
		{"", "on", oomCapOn},
		{"on", "off", oomCapOff}, // control kill switch beats an env "on"
		{"off", "on", oomCapOff}, // env off beats a control "on"
		{"shadow", "on", oomCapOn},
		{"on", "shadow", oomCapOn},
		{"", "off", oomCapOff},
		{"shadow", "shadow", oomCapShadow},
	}
	for _, c := range cases {
		withTempHome(t)
		t.Setenv("URNETWORK_OOM_CAP", c.env)
		prev := globalControlState
		globalControlState = newControlState()
		t.Cleanup(func() { globalControlState = prev })
		if c.control != "" {
			if err := globalControlState.set("oom_cap", c.control); err != nil {
				t.Fatal(err)
			}
		}
		if got := oomCapMode(); got != c.want {
			t.Errorf("env=%q control=%q: got %v, want %v", c.env, c.control, got, c.want)
		}
	}
}
