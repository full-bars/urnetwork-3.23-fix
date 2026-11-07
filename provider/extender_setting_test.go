package main

import (
	"context"
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

func TestExtenderUnrecognisedEnvWarnsOnceWhenOff(t *testing.T) {
	var logged []string
	prevHook := importantLogHook
	t.Cleanup(func() { importantLogHook = prevHook })
	importantLogHook = func(line string) {
		if strings.Contains(line, "ignoring URNETWORK_EXTENDER") {
			logged = append(logged, line)
		}
	}

	envVal := "banana"
	m := &extenderManager{
		environ: func(key string) string {
			if key == extenderEnv {
				return envVal
			}
			return ""
		},
		directEnabled: func() bool { return false },
		ctx:           context.Background(),
	}

	// First reconcile logs the warning
	m.Reconcile("check 1")
	if len(logged) != 1 || !strings.Contains(logged[0], "ignoring URNETWORK_EXTENDER=\"banana\"") {
		t.Fatalf("expected 1 warning line, got %v", logged)
	}

	// Second reconcile with same bad value must NOT repeat the warning
	m.Reconcile("check 2")
	if len(logged) != 1 {
		t.Fatalf("warning must be logged once for the same bad value, got %d times", len(logged))
	}

	// Changed bad value logs once
	envVal = "apple"
	m.Reconcile("check 3")
	if len(logged) != 2 || !strings.Contains(logged[1], "ignoring URNETWORK_EXTENDER=\"apple\"") {
		t.Fatalf("expected warning for apple, got %v", logged)
	}

	// Valid or empty value clears the marker
	envVal = ""
	m.Reconcile("check 4")
	if len(logged) != 2 {
		t.Fatalf("no warning expected for valid/empty value, got %d logs", len(logged))
	}

	// Re-introducing banana logs again
	envVal = "banana"
	m.Reconcile("check 5")
	if len(logged) != 3 {
		t.Fatalf("expected warning logged again after clearing, got %d logs", len(logged))
	}
}

func TestExtenderRoleRestartsWhenSupervisorStops(t *testing.T) {
	withTempHome(t)
	var logged []string
	prevHook := importantLogHook
	t.Cleanup(func() { importantLogHook = prevHook })
	importantLogHook = func(line string) {
		logged = append(logged, line)
	}

	roleStarts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newExtenderManager()
	m.ctx = ctx
	m.environ = func(string) string { return "" } // default "on"
	m.directEnabled = func() bool { return true }

	var currentRoleDone chan struct{}
	m.startRole = func(ctx context.Context, config extenderRoleConfig) (*extenderRole, error) {
		roleStarts++
		currentRoleDone = make(chan struct{})
		r := &extenderRole{
			done:   currentRoleDone,
			cancel: func() {},
		}
		return r, nil
	}

	// 1. Initial reconcile starts the first role
	m.Reconcile("startup")
	if roleStarts != 1 {
		t.Fatalf("roleStarts = %d, want 1", roleStarts)
	}
	firstDone := currentRoleDone

	// 2. While supervisor is still running, reconcile does not restart it
	m.Reconcile("periodic check 1")
	if roleStarts != 1 {
		t.Fatalf("roleStarts = %d, want 1 while role is alive", roleStarts)
	}

	// 3. Supervisor stops on its own (Done() closes)
	close(firstDone)

	// 4. Next reconcile must retire the dead role and start a replacement
	m.Reconcile("periodic check 2")
	if roleStarts != 2 {
		t.Fatalf("roleStarts = %d, want 2 after supervisor stopped", roleStarts)
	}
	if currentRoleDone == firstDone {
		t.Fatal("expected new role to be started")
	}

	foundLogged := false
	for _, l := range logged {
		if strings.Contains(l, "the role stopped on its own; starting it again") {
			foundLogged = true
			break
		}
	}
	if !foundLogged {
		t.Fatalf("expected restart line logged, got %v", logged)
	}
}
