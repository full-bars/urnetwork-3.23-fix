package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func installDefaultUserUnitForTest(t *testing.T) {
	t.Helper()
	configHome := t.TempDir()
	unitDir := filepath.Join(configHome, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "urnetwork.service"), []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := os.Getenv("XDG_CONFIG_HOME")
	if err := os.Setenv("XDG_CONFIG_HOME", configHome); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("XDG_CONFIG_HOME", original) })
}

func TestCmdStartUsesInstalledDefaultWhenDiscoveryTimesOut(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd fallback is Linux-only")
	}
	stubOpenRCForStartTest(t)
	stubDiscovery(t, nil, nil)
	installDefaultUserUnitForTest(t)

	originalInstalled := defaultUserUnitInstalledFn
	defaultUserUnitInstalledFn = func() bool { return true }
	t.Cleanup(func() { defaultUserUnitInstalledFn = originalInstalled })
	called := 0
	originalStart := startDefaultUserUnitFn
	startDefaultUserUnitFn = func() error { called++; return nil }
	t.Cleanup(func() { startDefaultUserUnitFn = originalStart })

	if err := cmdStart(nil, false, false); err != nil {
		t.Fatalf("cmdStart returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("default unit start called %d times, want 1", called)
	}
}

func TestCmdStartDoesNotGuessAfterDiscoveryTimeout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd fallback is Linux-only")
	}
	stubOpenRCForStartTest(t)
	stubDiscovery(t, nil, nil)
	originalInstalled := defaultUserUnitInstalledFn
	defaultUserUnitInstalledFn = func() bool { return false }
	t.Cleanup(func() { defaultUserUnitInstalledFn = originalInstalled })

	called := 0
	originalStart := startDefaultUserUnitFn
	startDefaultUserUnitFn = func() error { called++; return nil }
	t.Cleanup(func() { startDefaultUserUnitFn = originalStart })

	err := cmdStart(nil, false, false)
	if err == nil || !strings.Contains(err.Error(), "no providers found on this box") {
		t.Fatalf("cmdStart error = %v, want incomplete-discovery error", err)
	}
	if called != 0 {
		t.Fatalf("default unit start called %d times without an installed unit, want 0", called)
	}
}

func TestStartFallbackRequiresNoExplicitTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd fallback is Linux-only")
	}
	installDefaultUserUnitForTest(t)

	handled, err := startDefaultAfterEmptyDiscovery([]string{"--unit", "other.service"}, false, fmt.Errorf("no providers found on this box"))
	if err != nil || handled {
		t.Fatalf("explicit-target fallback = (%v, %v), want (false, nil)", handled, err)
	}
}
