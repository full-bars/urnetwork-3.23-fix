package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

// Restores the self-heal marker behavior: on/off writes ~/.urnetwork/proxy_self_heal
// and status reads it back. Uses $HOME override so it is hermetic.
func TestCmdSelfHealRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// status with no marker -> off
	if err := cmdSelfHeal([]string{"status"}); err != nil {
		t.Fatalf("status (no marker): %v", err)
	}
	marker := filepath.Join(home, ".urnetwork", "proxy_self_heal")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected no marker before 'on', but one exists")
	}

	// on -> marker exists with "on"
	if err := cmdSelfHeal([]string{"on"}); err != nil {
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
	if err := cmdSelfHeal([]string{"off"}); err != nil {
		t.Fatalf("off: %v", err)
	}
	b, _ = os.ReadFile(marker)
	if got := string(b); got != "off\n" {
		t.Fatalf("marker after off = %q, want off", got)
	}
}
