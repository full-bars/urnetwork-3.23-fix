package urnettools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `urnet-tools set tls-hello` must reach the provider as tls_hello, accept only
// on and off, and be named in `set help`: an operator who cannot find the key
// cannot flip it.
func TestTlsHelloKeyIsReachable(t *testing.T) {
	for _, alias := range []string{"tls-hello", "tls_hello", "TLS-Hello"} {
		canonical, ok := canonicalControlKey(alias)
		if !ok || canonical != "tls_hello" {
			t.Fatalf("canonicalControlKey(%q) = %q, %v; want tls_hello", alias, canonical, ok)
		}
	}
	for _, good := range []string{"on", "off", "ON", "Off"} {
		if err := validateControlValue("tls_hello", good); err != nil {
			t.Errorf("tls_hello=%q must be accepted: %v", good, err)
		}
	}
	for _, bad := range []string{"", "maybe", "1", "true", "yes", "chrome", "go"} {
		if err := validateControlValue("tls_hello", bad); err == nil {
			t.Errorf("tls_hello=%q must be rejected", bad)
		}
	}

	helpLines := 0
	for _, line := range setKeyHelps {
		if strings.HasPrefix(strings.TrimSpace(line), "tls-hello ") {
			helpLines++
		}
	}
	if helpLines == 0 {
		t.Error("setKeyHelps has no tls-hello line")
	}
}

// `set tls-hello off` is a value, not the generic clear: clearing hands the
// hello back to URNETWORK_TLS_HELLO, which may say on, so the kill switch would
// not survive a restart and the box would come back on the Chrome hello.
func TestTlsHelloOffIsSentAsAValueNotAClear(t *testing.T) {
	if treatsOffAsClear("tls_hello") {
		t.Fatal("tls_hello must NOT take the off-as-clear path: clearing returns to URNETWORK_TLS_HELLO, which may be on")
	}

	dir := t.TempDir()
	if err := applySetOverride(Provider{StateDir: dir}, "tls-hello", "off", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(data, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Op != "set" || ops[0].Key != "tls_hello" || ops[0].Value != "off" {
		t.Fatalf("queued ops = %+v, want one set tls_hello=off", ops)
	}
}
