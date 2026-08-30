package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

// Restores the self-heal marker behavior: on/off writes <state-dir>/proxy_self_heal
// and status reads it back. Mocks discovery so no running provider is needed.
func TestCmdSelfHealRoundTrip(t *testing.T) {
	stateDir := t.TempDir()

	// Mock discovery to return a provider with our temp state dir.
	origDiscover := discoverSystemdFn
	discoverSystemdFn = func() []Provider {
		return []Provider{{
			User:     "test-user",
			StateDir: stateDir,
			Unit:     "urnetwork.service",
			Running:  true,
		}}
	}
	defer func() { discoverSystemdFn = origDiscover }()

	// status with no marker -> off
	if err := cmdSelfHeal([]string{"status", "--state-dir", stateDir}); err != nil {
		t.Fatalf("status (no marker): %v", err)
	}
	marker := filepath.Join(stateDir, "proxy_self_heal")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected no marker before 'on', but one exists")
	}

	// on -> marker exists with "on"
	if err := cmdSelfHeal([]string{"on", "--state-dir", stateDir}); err != nil {
		t.Fatalf("on: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := string(b); got != "on\n" {
		t.Fatalf("marker = %q, want on", got)
	}

	// off -> marker now "off"
	if err := cmdSelfHeal([]string{"off", "--state-dir", stateDir}); err != nil {
		t.Fatalf("off: %v", err)
	}
	b, err = os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := string(b); got != "off\n" {
		t.Fatalf("marker = %q, want off", got)
	}
}
