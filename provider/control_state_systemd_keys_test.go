package main

import (
	"runtime/debug"
	"testing"
)

// TestControlState_SystemdMigratedKeysAccepted covers the 5 keys migrated
// off systemd override.conf (see the comment on controlKeys): they must be
// valid set/clear targets exactly like the original 9.
func TestControlState_SystemdMigratedKeysAccepted(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	for _, key := range []string{"hot_restart", "gomemlimit", "gogc", "profile", "ramlogs"} {
		if err := s.set(key, "on"); err != nil {
			t.Errorf("set(%s): unexpected error: %v", key, err)
		}
		if _, found := s.get(key); !found {
			t.Errorf("get(%s): expected found after set", key)
		}
		if err := s.clear(key); err != nil {
			t.Errorf("clear(%s): unexpected error: %v", key, err)
		}
	}
}

func TestHotRestartEnabled_ControlSocketOverride(t *testing.T) {
	withTempHome(t)

	if !hotRestartEnabled() {
		t.Fatalf("expected on by default with nothing set")
	}

	globalControlState.set("hot_restart", "off")
	if hotRestartEnabled() {
		t.Fatalf("expected off after socket set to off")
	}

	globalControlState.set("hot_restart", "on")
	if !hotRestartEnabled() {
		t.Fatalf("expected on after socket set to on")
	}

	globalControlState.clear("hot_restart")
	if !hotRestartEnabled() {
		t.Fatalf("expected fall-through to env-var default (on) after clear")
	}
}

func TestHotRestartEnabled_EnvVarFallbackUnchanged(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_HOT_RESTART", "0")

	if hotRestartEnabled() {
		t.Fatalf("expected URNETWORK_HOT_RESTART=0 to still disable it when nothing is set via the socket")
	}
}

// TestApplyLiveSideEffect_GOGC exercises the actual runtime/debug call, not
// just that no error is returned — restores the original GC percent after
// so this test doesn't leak a changed GC setting into the rest of the
// binary's test run.
func TestApplyLiveSideEffect_GOGC(t *testing.T) {
	original := debug.SetGCPercent(100)
	defer debug.SetGCPercent(original)

	if err := applyLiveSideEffect("gogc", "42"); err != nil {
		t.Fatalf("applyLiveSideEffect: %v", err)
	}
	got := debug.SetGCPercent(100) // SetGCPercent returns the previous value
	if got != 42 {
		t.Fatalf("GOGC after apply = %d, want 42", got)
	}
}

func TestApplyLiveSideEffect_GOMemLimit(t *testing.T) {
	original := debug.SetMemoryLimit(-1) // -1 reads without changing
	defer debug.SetMemoryLimit(original)

	if err := applyLiveSideEffect("gomemlimit", "256MiB"); err != nil {
		t.Fatalf("applyLiveSideEffect: %v", err)
	}
	got := debug.SetMemoryLimit(-1)
	want := int64(256 * 1024 * 1024)
	if got != want {
		t.Fatalf("GOMEMLIMIT after apply = %d, want %d", got, want)
	}
}

func TestApplyLiveSideEffect_InvalidValueErrors(t *testing.T) {
	if err := applyLiveSideEffect("gogc", "not-a-number"); err == nil {
		t.Fatalf("expected error for non-numeric gogc value")
	}
	if err := applyLiveSideEffect("gomemlimit", "not-a-size"); err == nil {
		t.Fatalf("expected error for unparseable gomemlimit value")
	}
}

// TestApplyLiveSideEffect_OtherKeysAreNoop confirms keys with no live
// side effect (everything except gomemlimit/gogc) don't error — they're
// meant to only take effect via the persisted value's next read.
func TestApplyLiveSideEffect_OtherKeysAreNoop(t *testing.T) {
	for _, key := range []string{"node_name", "hot_restart", "profile", "ramlogs"} {
		if err := applyLiveSideEffect(key, "anything"); err != nil {
			t.Errorf("applyLiveSideEffect(%s): expected no-op, got error: %v", key, err)
		}
	}
}

// TestControlSocket_SetGOGCEndToEnd is the full-stack version of the GOGC
// test above: through the actual socket, not calling applyLiveSideEffect
// directly, confirming handleControlRequest wires it in on the set path.
func TestControlSocket_SetGOGCEndToEnd(t *testing.T) {
	withTempHome(t)
	globalControlState = newControlState()
	original := debug.SetGCPercent(100)
	defer debug.SetGCPercent(original)

	resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "gogc", Value: "77"})
	if !resp.OK {
		t.Fatalf("set response: %+v", resp)
	}
	got := debug.SetGCPercent(100)
	if got != 77 {
		t.Fatalf("GOGC after socket set = %d, want 77", got)
	}
}
