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
	// A successful set logs an acknowledgement to the disk event log; keep that
	// (and everything else the handler touches) in a temp home.
	withTempHome(t)
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

// The control-socket ack must report the mode that actually governs, not just
// the value the operator typed: URNETWORK_OOM_CAP=on always beats a persisted
// "shadow" or "off" (see oomCapMode's precedence), so an ack that echoed only
// the requested value silently misled an operator into thinking a
// control-socket "off" disabled enforcement when the env var kept it on.
func TestOOMCapControlAckReportsEffectiveMode(t *testing.T) {
	withTempHome(t)
	prev := globalControlState
	t.Cleanup(func() { globalControlState = prev })

	t.Run("no env override: ack matches the request", func(t *testing.T) {
		t.Setenv("URNETWORK_OOM_CAP", "")
		globalControlState = newControlState()
		out := captureTlog(t, func() {
			resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: "shadow"})
			if !resp.OK {
				t.Fatalf("set rejected: %s", resp.Error)
			}
		})
		if !strings.Contains(out, "mode set to shadow via control socket (the automatic") {
			t.Fatalf("expected a plain ack with no override note, got:\n%s", out)
		}
		if strings.Contains(out, "overrides it") {
			t.Fatalf("no env is set: must not claim an override, got:\n%s", out)
		}
	})

	t.Run("env on overrides a control-socket shadow", func(t *testing.T) {
		t.Setenv("URNETWORK_OOM_CAP", "on")
		globalControlState = newControlState()
		out := captureTlog(t, func() {
			resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: "shadow"})
			if !resp.OK {
				t.Fatalf("set rejected: %s", resp.Error)
			}
		})
		if !strings.Contains(out, "mode set to shadow via control socket, but URNETWORK_OOM_CAP env overrides it: effective mode is on") {
			t.Fatalf("expected the ack to name the effective mode, got:\n%s", out)
		}
	})
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
