package main

import (
	"testing"

	connect "github.com/urnetwork/connect"
)

// tls_hello is a control key and a live one: without the controlKeys entry
// set/clear reject it, and without the liveEffectKeys entry every change would
// claim to need a restart it does not need. The gate it drives is what the
// dialers read, so these tests assert connect's gate, not a provider field.
func TestTlsHelloKeyIsLive(t *testing.T) {
	if !controlKeys["tls_hello"] {
		t.Fatal("controlKeys has no tls_hello entry: set/clear would reject it and it could never persist")
	}
	if needsRestart("tls_hello") {
		t.Fatal("tls_hello is applied live: needsRestart must be false")
	}
}

// Nothing but on and off is a hello. The provider's server-side validation and
// urnet-tools' client-side table must agree, and neither accepts the loose
// on-ish spellings the on|off env vars tolerate (URNETWORK_TLS_HELLO=1 is a
// startup convenience, not a value the socket persists).
func TestTlsHelloValidation(t *testing.T) {
	for _, good := range []string{"on", "off", "ON", "Off", "oFf"} {
		if err := validateControlValue("tls_hello", good); err != nil {
			t.Errorf("validateControlValue(tls_hello, %q) = %v, want accepted", good, err)
		}
	}
	for _, bad := range []string{"", "maybe", "1", "true", "yes", "0", "on ", " on", "chrome", "go"} {
		if err := validateControlValue("tls_hello", bad); err == nil {
			t.Errorf("validateControlValue(tls_hello, %q) accepted a value that is not on or off", bad)
		}
	}
}

// A set applies at once, in both directions, and a clear hands the decision
// back to URNETWORK_TLS_HELLO: a box whose environment asks for the Chrome
// hello follows it again, and one without the variable goes back to Go's hello.
func TestTlsHelloAppliesLiveAndClearsToEnv(t *testing.T) {
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		connect.SetTlsClientHelloChrome(false)
	})

	t.Setenv("URNETWORK_TLS_HELLO", "")
	globalControlState = newControlState()

	if err := applyLiveSideEffect("tls_hello", "on"); err != nil || !connect.TlsClientHelloChrome() {
		t.Fatalf("set on: err=%v chrome=%v", err, connect.TlsClientHelloChrome())
	}
	if err := applyLiveSideEffect("tls_hello", "off"); err != nil || connect.TlsClientHelloChrome() {
		t.Fatalf("set off: err=%v chrome=%v", err, connect.TlsClientHelloChrome())
	}

	t.Setenv("URNETWORK_TLS_HELLO", "on")
	if err := applyLiveDefault("tls_hello"); err != nil || !connect.TlsClientHelloChrome() {
		t.Fatalf("clear with URNETWORK_TLS_HELLO=on: err=%v chrome=%v", err, connect.TlsClientHelloChrome())
	}
	t.Setenv("URNETWORK_TLS_HELLO", "")
	if err := applyLiveDefault("tls_hello"); err != nil || connect.TlsClientHelloChrome() {
		t.Fatalf("clear with URNETWORK_TLS_HELLO unset: err=%v chrome=%v", err, connect.TlsClientHelloChrome())
	}
}

// The startup replay puts the gate where the setting says, and it runs in both
// directions: an unset key follows URNETWORK_TLS_HELLO (on and off), and a
// persisted value beats the environment either way. Leaving the gate wherever a
// previous run left it would make a restart's hello depend on history.
func TestTlsHelloStartupReplay(t *testing.T) {
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		connect.SetTlsClientHelloChrome(false)
	})

	state := newControlState()
	globalControlState = state

	// unset key, environment on: the gate must be on at startup
	t.Setenv("URNETWORK_TLS_HELLO", "on")
	connect.SetTlsClientHelloChrome(false)
	applyPersistedRuntimeTuning(state)
	if !connect.TlsClientHelloChrome() {
		t.Fatal("unset key with URNETWORK_TLS_HELLO=on: the gate must follow the environment at startup")
	}

	// unset key, environment off: the gate must go back off, not stay where the
	// previous replay left it
	t.Setenv("URNETWORK_TLS_HELLO", "")
	connect.SetTlsClientHelloChrome(true)
	applyPersistedRuntimeTuning(state)
	if connect.TlsClientHelloChrome() {
		t.Fatal("unset key with URNETWORK_TLS_HELLO unset: the gate must return to Go's hello at startup")
	}

	// a persisted on is replayed even with the environment asking for Go's hello
	t.Setenv("URNETWORK_TLS_HELLO", "")
	if err := state.set("tls_hello", "on"); err != nil {
		t.Fatal(err)
	}
	connect.SetTlsClientHelloChrome(false)
	applyPersistedRuntimeTuning(state)
	if !connect.TlsClientHelloChrome() {
		t.Fatal("a persisted on must be replayed at startup")
	}

	// and a persisted off beats an environment that says on
	t.Setenv("URNETWORK_TLS_HELLO", "on")
	if err := state.set("tls_hello", "off"); err != nil {
		t.Fatal(err)
	}
	connect.SetTlsClientHelloChrome(true)
	applyPersistedRuntimeTuning(state)
	if connect.TlsClientHelloChrome() {
		t.Fatal("a persisted off must beat URNETWORK_TLS_HELLO=on at startup")
	}
}

// Through the socket's own set and clear commands, which is what an operator
// runs: the value persists, applying it needs no restart, and clearing it
// returns to URNETWORK_TLS_HELLO rather than to a hardcoded default.
func TestTlsHelloControlSocketSetAndClear(t *testing.T) {
	withTempHome(t)
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		connect.SetTlsClientHelloChrome(false)
	})

	state := newControlState()
	globalControlState = state
	t.Setenv("URNETWORK_TLS_HELLO", "on")

	resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "tls_hello", Value: "on"})
	if !resp.OK || resp.NeedsRestart {
		t.Fatalf("set on: ok=%v needs_restart=%v error=%q", resp.OK, resp.NeedsRestart, resp.Error)
	}
	if !connect.TlsClientHelloChrome() {
		t.Fatal("set on did not turn the Chrome hello on")
	}
	if v, ok := state.get("tls_hello"); !ok || v != "on" {
		t.Fatalf("set on persisted %q, %v", v, ok)
	}

	// off is a real value, not a clear: on a box whose environment says on, a
	// clear would come back up with the Chrome hello
	resp = handleControlRequest(state, controlRequest{Cmd: "set", Key: "tls_hello", Value: "off"})
	if !resp.OK || resp.NeedsRestart {
		t.Fatalf("set off: ok=%v needs_restart=%v error=%q", resp.OK, resp.NeedsRestart, resp.Error)
	}
	if connect.TlsClientHelloChrome() {
		t.Fatal("set off left the Chrome hello on")
	}
	if v, ok := state.get("tls_hello"); !ok || v != "off" {
		t.Fatalf("set off persisted %q, %v", v, ok)
	}

	resp = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "tls_hello"})
	if !resp.OK || resp.NeedsRestart {
		t.Fatalf("clear: ok=%v needs_restart=%v error=%q", resp.OK, resp.NeedsRestart, resp.Error)
	}
	if !connect.TlsClientHelloChrome() {
		t.Fatal("clear on a box with URNETWORK_TLS_HELLO=on must follow the environment")
	}
	if _, ok := state.get("tls_hello"); ok {
		t.Fatal("clear left the key in the persisted state")
	}
}
