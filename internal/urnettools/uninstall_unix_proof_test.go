//go:build linux

package urnettools

import (
	"os"
	"testing"
)

func stubPidAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	orig := uninstallPidAliveFn
	uninstallPidAliveFn = fn
	t.Cleanup(func() { uninstallPidAliveFn = orig })
}

// A successful stop command is not proof of exit: a pid that is still
// alive (or cannot be inspected) must block removal.
func TestUnixUninstallAbortsWhenPidSurvivesStop(t *testing.T) {
	f := newUninstallFixture(t, true)
	uninstallStopsProcesses = false
	f.socketUp = func() bool { return false }
	stubPidAlive(t, func(int) bool { return true })
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "still running")
}

func TestUnixUninstallRemovesFilesOnceProvenGone(t *testing.T) {
	f := newUninstallFixture(t, true)
	uninstallStopsProcesses = false
	f.socketUp = func() bool { return false }
	stubPidAlive(t, func(int) bool { return false })
	if err := cmdUninstall(nil, true, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if f.filesPresent() {
		t.Errorf("files still present after a proven stop")
	}
}

func TestProcessAliveStrict(t *testing.T) {
	if !processAliveStrict(os.Getpid()) {
		t.Error("own pid must be alive")
	}
	if processAliveStrict(0) || processAliveStrict(-1) {
		t.Error("non-positive pids are not processes")
	}
	if processAliveStrict(1<<22 + 12345) {
		t.Error("an unused pid must read as gone (ESRCH)")
	}
}
