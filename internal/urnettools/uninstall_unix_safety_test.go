//go:build linux

package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The Unix path must not remove a live provider's files when stopping fails.
func TestUnixUninstallStopFailurePreservesFiles(t *testing.T) {
	f := newUninstallFixture(t, true)
	uninstallStopsProcesses = false
	f.stopErr = errors.New("stop denied")
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "could not stop")
}

func TestUnixUninstallSystemdFailurePreservesFiles(t *testing.T) {
	f := newUninstallFixture(t, true)
	uninstallStopsProcesses = false
	f.p.Unit = "urnetwork.service"
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "disable")
}
