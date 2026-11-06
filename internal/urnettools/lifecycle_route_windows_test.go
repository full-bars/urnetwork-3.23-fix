//go:build windows

package urnettools

import (
	"strings"
	"testing"
)

// stubWindowsLifecycleTarget makes discovery return one disposable provider
// owned by the caller, so cmdStart and cmdRestart reach their Windows route
// without looking at the real profile.
func stubWindowsLifecycleTarget(t *testing.T, pid int) Provider {
	t.Helper()
	p := Provider{User: currentUserName(), StateDir: t.TempDir(), Binary: `C:\nonexistent\urnetwork.exe`, PID: pid, Running: pid > 0}
	origDiscover, origDocker := discoverSystemdFn, discoverDockerFn
	t.Cleanup(func() { discoverSystemdFn, discoverDockerFn = origDiscover, origDocker })
	discoverSystemdFn = func() []Provider { return []Provider{p} }
	discoverDockerFn = func() []Provider { return nil }
	return p
}

// The start command must go through the already-running check on Windows.
func TestCmdStartWindowsDoesNotLaunchSecondCopy(t *testing.T) {
	stubWindowsLifecycleTarget(t, 4242)
	launches := stubWindowsStart(t, withProcesses(targetProc), false)
	var err error
	out := captureStdout(t, func() { err = cmdStart(nil, true, false) })
	if err != nil {
		t.Fatalf("cmdStart: %v", err)
	}
	if *launches != 0 {
		t.Fatalf("cmdStart launched a second copy")
	}
	if !strings.Contains(out, "already running") {
		t.Fatalf("output %q, want an already-running note", out)
	}
}

// The restart command must refuse on Windows while another provider process
// may own the control socket.
func TestCmdRestartWindowsRefusesSharedProfile(t *testing.T) {
	stubWindowsLifecycleTarget(t, 4242)
	calls := stubWindowsRestart(t, true, nil)
	listProviderProcessesFn = withProcesses(targetProc, sharingProc)
	err := cmdRestart(nil, true, false)
	if err == nil || !strings.Contains(err.Error(), "pid 5151") {
		t.Fatalf("err = %v, want a refusal naming pid 5151", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("restart stop/start ran (calls %v)", *calls)
	}
}
