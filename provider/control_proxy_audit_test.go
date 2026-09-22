package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// F1 regression: proxy_audit must be a full control key so that
// `urnet-tools proxy audit on|off` persists through the set transaction
// instead of reverting to observe mode on the next restart. The key was
// missing from controlKeys, making the store reject the set silently and
// leaving only the in-memory override live.
func TestControlState_ProxyAuditKeyLive(t *testing.T) {
	if !controlKeys["proxy_audit"] {
		t.Fatal("controlKeys has no proxy_audit entry: proxy audit on/off would never persist (reverts on restart)")
	}
	if !liveEffectKeys["proxy_audit"] {
		t.Fatal("liveEffectKeys has no proxy_audit entry: toggling would require a restart")
	}
	if got := liveDefaults["proxy_audit"]; !strings.EqualFold(got, "off") {
		t.Errorf("liveDefaults[proxy_audit] = %q, want \"off\" (clear must reapply observe mode)", got)
	}
}

// TestControlKeys_LiveInvariants is the class-level guard for the F1 bug:
// every key blessed for live side effects or a live default must be a real
// control key, or set/clear reject it and the live wiring is unreachable.
// proxy_audit was in liveEffectKeys and validateControlValue but missing
// from controlKeys, and nothing caught it; this loop fails on exactly that.
func TestControlKeys_LiveInvariants(t *testing.T) {
	for k := range liveEffectKeys {
		if !controlKeys[k] {
			t.Errorf("liveEffectKeys includes %q which is not a control key: set/clear reject it, live effect unreachable", k)
		}
	}
	for k := range liveDefaults {
		if !liveEffectKeys[k] {
			t.Errorf("liveDefaults includes %q which has no live effect key: clear cannot reapply it", k)
		}
		if !controlKeys[k] {
			t.Errorf("liveDefaults includes %q which is not a control key", k)
		}
	}
}

func TestControlState_ProxyAuditValidateAndRoundtrip(t *testing.T) {
	if err := validateControlValue("proxy_audit", "on"); err != nil {
		t.Fatalf("validateControlValue(proxy_audit, on): %v", err)
	}
	if err := validateControlValue("proxy_audit", "OFF"); err != nil {
		t.Fatalf("validateControlValue(proxy_audit, OFF): %v", err)
	}
	if err := validateControlValue("proxy_audit", "bogus"); err == nil {
		t.Fatal("validateControlValue(proxy_audit, bogus) accepted an invalid value")
	}

	s := newControlState()
	if err := s.set("proxy_audit", "on"); err != nil {
		t.Fatalf("set(proxy_audit, on): %v", err)
	}
	v, ok := s.get("proxy_audit")
	if !ok || v != "on" {
		t.Fatalf("get after set: %q, %v; want on, true", v, ok)
	}
	if err := s.clear("proxy_audit"); err != nil {
		t.Fatalf("clear(proxy_audit): %v", err)
	}
}

// F2 regression: the parent's control-socket cleanup is idempotent. The
// quiesce at the hotswap commit point removes the socket; a second call on
// the graceful-exit path must not delete the SUCCESSOR's socket, which it
// has since bound at the same path. This test reproduces exactly that
// sequence: parent binds, quiesces, successor binds the same path, parent
// cleans up again, and the successor's socket must survive.
func TestControlSocketCleanup_IdempotentAcrossSuccessor(t *testing.T) {
	oldHome := os.Getenv("HOME")
	home := t.TempDir()
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	ctx := context.Background()
	state := newControlState()

	parentCleanup, err := startControlSocket(ctx, state)
	if err != nil {
		t.Fatalf("parent startControlSocket: %v", err)
	}
	parentCleanup() // quiesce at the handoff commit point

	successorCleanup, err := startControlSocket(ctx, state)
	if err != nil {
		t.Fatalf("successor startControlSocket: %v", err)
	}
	defer successorCleanup()

	// The parent's graceful-exit path runs its cleanup again. Idempotent
	// cleanup makes this a no-op; a non-idempotent one deletes the
	// successor's socket and locks the operator out until a restart.
	parentCleanup()

	path, err := controlSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("parent's second cleanup deleted the successor's socket at %s: %v", path, err)
	}
}
