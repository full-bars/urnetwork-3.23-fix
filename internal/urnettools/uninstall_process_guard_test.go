package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// uninstallFixture wires cmdUninstall to a disposable provider: a temp
// binary and state dir, and seams for the process list, the stop, the
// control socket and the lifecycle cleanup. Nothing real is listed,
// stopped or deleted outside t.TempDir().
type uninstallFixture struct {
	p        Provider
	events   []string
	procs    func() ([]providerProcess, error)
	stopErr  error
	socketUp func() bool
	stopped  bool
	sleeps   int
	t        *testing.T
}

func newUninstallFixture(t *testing.T, running bool) *uninstallFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "provider", "urnetwork.exe")
	state := filepath.Join(dir, ".urnetwork")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "jwt"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &uninstallFixture{t: t, p: Provider{
		User:     currentUserName(),
		StateDir: state,
		Binary:   bin,
		Running:  running,
	}}
	if running {
		f.p.PID = 4242
	}
	// Default: the target runs until stopped, then disappears.
	f.procs = func() ([]providerProcess, error) {
		if running && !f.stopped {
			return []providerProcess{{PID: 4242, Exe: bin, OwnerKnown: true, OwnedByCaller: true}}, nil
		}
		return nil, nil
	}
	f.socketUp = func() bool { return running && !f.stopped }

	origDiscover, origDocker := discoverSystemdFn, discoverDockerFn
	origGuard, origList, origStop := uninstallStopsProcesses, listProviderProcessesFn, uninstallStopFn
	origSocket, origSleep, origCleanup := uninstallSocketReachableFn, uninstallSleepFn, cleanupLifecycleFn
	t.Cleanup(func() {
		discoverSystemdFn, discoverDockerFn = origDiscover, origDocker
		uninstallStopsProcesses, listProviderProcessesFn, uninstallStopFn = origGuard, origList, origStop
		uninstallSocketReachableFn, uninstallSleepFn, cleanupLifecycleFn = origSocket, origSleep, origCleanup
	})

	discoverSystemdFn = func() []Provider { return []Provider{f.p} }
	discoverDockerFn = func() []Provider { return nil }
	uninstallStopsProcesses = true
	listProviderProcessesFn = func() ([]providerProcess, error) { return f.procs() }
	uninstallStopFn = func(Provider) error {
		f.events = append(f.events, "stop")
		if !f.filesPresent() {
			t.Errorf("provider files were removed before the stop")
		}
		if f.stopErr != nil {
			return f.stopErr
		}
		f.stopped = true
		return nil
	}
	uninstallSocketReachableFn = func(Provider) bool { return f.socketUp() }
	uninstallSleepFn = func(time.Duration) { f.sleeps++ }
	cleanupLifecycleFn = func(Provider) { f.events = append(f.events, "cleanup") }
	return f
}

func (f *uninstallFixture) filesPresent() bool {
	_, berr := os.Stat(f.p.Binary)
	_, serr := os.Stat(filepath.Join(f.p.StateDir, "jwt"))
	return berr == nil && serr == nil
}

func (f *uninstallFixture) assertUntouched(err error, wantMsg string) {
	f.t.Helper()
	if err == nil {
		f.t.Fatalf("uninstall succeeded, want an abort mentioning %q", wantMsg)
	}
	if !strings.Contains(err.Error(), wantMsg) || !strings.Contains(err.Error(), "nothing was") {
		f.t.Errorf("error = %q, want it to mention %q and that nothing was removed", err, wantMsg)
	}
	if !f.filesPresent() {
		f.t.Errorf("binary or state dir was removed despite the abort")
	}
	for _, e := range f.events {
		if e == "cleanup" {
			f.t.Errorf("lifecycle cleanup ran despite the abort (events %v)", f.events)
		}
	}
}

func TestUninstallStopsRunningProviderBeforeRemovingFiles(t *testing.T) {
	f := newUninstallFixture(t, true)
	if err := cmdUninstall(nil, true, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := strings.Join(f.events, ","); got != "stop,cleanup" {
		t.Errorf("events = %q, want stop before cleanup", got)
	}
	if _, err := os.Stat(f.p.Binary); !os.IsNotExist(err) {
		t.Errorf("binary still present after uninstall: %v", err)
	}
	if _, err := os.Stat(f.p.StateDir); !os.IsNotExist(err) {
		t.Errorf("state dir still present after uninstall: %v", err)
	}
}

func TestUninstallSkipsStopWhenProviderNotRunning(t *testing.T) {
	f := newUninstallFixture(t, false)
	if err := cmdUninstall(nil, true, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := strings.Join(f.events, ","); got != "cleanup" {
		t.Errorf("events = %q, want cleanup only (nothing to stop)", got)
	}
	if f.filesPresent() {
		t.Errorf("files not removed for a stopped provider")
	}
}

func TestUninstallAbortsWhenStopFails(t *testing.T) {
	f := newUninstallFixture(t, true)
	f.stopErr = errors.New("access denied")
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "could not stop")
}

func TestUninstallAbortsWhenProviderDoesNotExit(t *testing.T) {
	f := newUninstallFixture(t, true)
	// The stop "succeeds" but the process never leaves the list.
	f.procs = func() ([]providerProcess, error) {
		return []providerProcess{{PID: 4242, Exe: f.p.Binary, OwnerKnown: true, OwnedByCaller: true}}, nil
	}
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "still running after the stop")
	if f.sleeps == 0 {
		t.Errorf("uninstall gave up without waiting for the exit")
	}
}

func TestUninstallAbortsWhenSocketStillAnswers(t *testing.T) {
	f := newUninstallFixture(t, true)
	f.socketUp = func() bool { return true }
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "still answers on")
}

func TestUninstallFailsClosedWhenProcessListFails(t *testing.T) {
	f := newUninstallFixture(t, true)
	f.procs = func() ([]providerProcess, error) { return nil, errors.New("snapshot failed") }
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "could not list running provider processes")
	if len(f.events) != 0 {
		t.Errorf("events = %v, want nothing stopped when the list fails", f.events)
	}
}

func TestUninstallLeavesAnotherLiveProviderAlone(t *testing.T) {
	f := newUninstallFixture(t, true)
	other := providerProcess{PID: 777, Exe: `C:\beta\provider_beta.exe`, OwnerKnown: true, OwnedByCaller: true}
	f.procs = func() ([]providerProcess, error) {
		out := []providerProcess{other}
		if !f.stopped {
			out = append(out, providerProcess{PID: 4242, Exe: f.p.Binary, OwnerKnown: true, OwnedByCaller: true})
		}
		return out, nil
	}
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "another provider process is running")
	if len(f.events) != 0 {
		t.Errorf("events = %v, want the target left running too (nothing stopped)", f.events)
	}
}

func TestUninstallTreatsUnknownOwnerAsSharing(t *testing.T) {
	f := newUninstallFixture(t, true)
	f.procs = func() ([]providerProcess, error) {
		return []providerProcess{
			{PID: 4242, Exe: f.p.Binary, OwnerKnown: true, OwnedByCaller: true},
			{PID: 888, Exe: "urnetwork.exe"},
		}, nil
	}
	err := cmdUninstall(nil, true, false)
	f.assertUntouched(err, "owner unknown")
}

func TestUninstallIgnoresAnotherAccountsProvider(t *testing.T) {
	f := newUninstallFixture(t, true)
	other := providerProcess{PID: 888, Exe: `C:\Users\other\AppData\Local\urnetwork\provider\urnetwork.exe`, OwnerKnown: true}
	f.procs = func() ([]providerProcess, error) {
		out := []providerProcess{other}
		if !f.stopped {
			out = append(out, providerProcess{PID: 4242, Exe: f.p.Binary, OwnerKnown: true, OwnedByCaller: true})
		}
		return out, nil
	}
	if err := cmdUninstall(nil, true, false); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := strings.Join(f.events, ","); got != "stop,cleanup" {
		t.Errorf("events = %q, want stop,cleanup", got)
	}
}

func TestUninstallDryRunTouchesNothing(t *testing.T) {
	f := newUninstallFixture(t, true)
	if err := cmdUninstall(nil, true, true); err != nil {
		t.Fatalf("dry-run uninstall: %v", err)
	}
	if len(f.events) != 0 || !f.filesPresent() {
		t.Errorf("dry run acted: events %v, files present %v", f.events, f.filesPresent())
	}
}
