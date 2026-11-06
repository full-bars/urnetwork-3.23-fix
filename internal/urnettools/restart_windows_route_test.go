package urnettools

import (
	"errors"
	"strings"
	"testing"
)

// stubWindowsRestart stubs every seam the Windows restart routes touch. By
// default the raw process list shows only the target (pid 4242) while it is
// alive; tests that need another process or a list failure overwrite
// listProviderProcessesFn afterwards.
func stubWindowsRestart(t *testing.T, alive bool, restartErr error) *[]int {
	t.Helper()
	origAlive, origRestart := restartWindowsAliveFn, restartProviderWindowsFn
	origCmd, origList := restartWindowsCmdFn, listProviderProcessesFn
	t.Cleanup(func() {
		restartWindowsAliveFn, restartProviderWindowsFn = origAlive, origRestart
		restartWindowsCmdFn, listProviderProcessesFn = origCmd, origList
	})
	var calls []int
	restartWindowsAliveFn = func(int) bool { return alive }
	restartProviderWindowsFn = func(p Provider) error {
		calls = append(calls, p.PID)
		return restartErr
	}
	restartWindowsCmdFn = func(p Provider, _, _ bool) error {
		calls = append(calls, p.PID)
		return restartErr
	}
	listProviderProcessesFn = func() ([]providerProcess, error) {
		if !alive {
			return nil, nil
		}
		return []providerProcess{targetProc}, nil
	}
	return &calls
}

func withProcesses(procs ...providerProcess) func() ([]providerProcess, error) {
	return func() ([]providerProcess, error) { return procs, nil }
}

var (
	targetProc   = providerProcess{PID: 4242, Exe: "urnetwork.exe", OwnerKnown: true, OwnedByCaller: true}
	sharingProc  = providerProcess{PID: 5151, Exe: "urnetwork.exe", OwnerKnown: true, OwnedByCaller: true}
	unknownProc  = providerProcess{PID: 6262, Exe: "urnetwork.exe"}
	otherAcctPrc = providerProcess{PID: 7373, Exe: "urnetwork.exe", OwnerKnown: true, OwnedByCaller: false}
)

func TestRestartProviderOnWindowsRestartsLiveProvider(t *testing.T) {
	calls := stubWindowsRestart(t, true, nil)
	if err := restartProviderOnWindows(Provider{PID: 4242, StateDir: t.TempDir()}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != 4242 {
		t.Fatalf("stop/start calls = %v, want one for pid 4242", *calls)
	}
}

func TestRestartProviderOnWindowsPropagatesFailure(t *testing.T) {
	stubWindowsRestart(t, true, errors.New("start: boom"))
	err := restartProviderOnWindows(Provider{PID: 4242})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the stop/start failure", err)
	}
}

func TestRestartProviderOnWindowsLeavesStoppedProviderStopped(t *testing.T) {
	for _, p := range []Provider{{PID: 0}, {PID: 4242}} {
		calls := stubWindowsRestart(t, false, nil)
		err := restartProviderOnWindows(p)
		if err == nil || !strings.Contains(err.Error(), "could not restart provider") || !strings.Contains(err.Error(), "not running") {
			t.Errorf("pid %d: err = %v, want a not-running refusal", p.PID, err)
		}
		if len(*calls) != 0 {
			t.Errorf("pid %d: a stopped provider was started (calls %v)", p.PID, *calls)
		}
	}
}

// Two provider processes can share one state dir on Windows. The control
// socket may then belong to the other one, and the stop half of a restart
// would shut that provider down instead. Both restart routes (update/tune
// and the restart command) must refuse before stopping anything.
func TestWindowsRestartRefusesWhileAnotherProviderSharesProfile(t *testing.T) {
	routes := []struct {
		name  string
		route func(Provider) error
	}{
		{"update restart", restartProviderOnWindows},
		{"restart command", func(p Provider) error { return cmdRestartOnWindows(p, false, false) }},
	}
	for _, r := range routes {
		t.Run(r.name+"/same user", func(t *testing.T) {
			calls := stubWindowsRestart(t, true, nil)
			listProviderProcessesFn = withProcesses(targetProc, sharingProc)
			err := r.route(Provider{PID: 4242, StateDir: `C:\Users\u\.urnetwork`})
			if err == nil {
				t.Fatalf("restart went ahead while pid 5151 shares the profile")
			}
			for _, want := range []string{"pid 5151", "nothing was stopped", "Task Manager", "restart"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to mention %q", err, want)
				}
			}
			if len(*calls) != 0 {
				t.Fatalf("stop/start ran (calls %v)", *calls)
			}
		})
		t.Run(r.name+"/unknown owner counts", func(t *testing.T) {
			calls := stubWindowsRestart(t, true, nil)
			listProviderProcessesFn = withProcesses(targetProc, unknownProc)
			err := r.route(Provider{PID: 4242, StateDir: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "pid 6262") || !strings.Contains(err.Error(), "run restart from an elevated prompt") {
				t.Fatalf("err = %v, want a refusal naming pid 6262 and the elevated-prompt hint", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("stop/start ran (calls %v)", *calls)
			}
		})
		t.Run(r.name+"/other account ignored", func(t *testing.T) {
			calls := stubWindowsRestart(t, true, nil)
			listProviderProcessesFn = withProcesses(targetProc, otherAcctPrc)
			if err := r.route(Provider{PID: 4242, StateDir: t.TempDir()}); err != nil {
				t.Fatalf("restart: %v", err)
			}
			if len(*calls) != 1 {
				t.Fatalf("stop/start calls = %v, want 1", *calls)
			}
		})
		t.Run(r.name+"/list failure fails closed", func(t *testing.T) {
			calls := stubWindowsRestart(t, true, nil)
			listProviderProcessesFn = func() ([]providerProcess, error) { return nil, errors.New("snapshot denied") }
			err := r.route(Provider{PID: 4242, StateDir: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "snapshot denied") || !strings.Contains(err.Error(), "nothing was stopped") {
				t.Fatalf("err = %v, want a fail-closed refusal carrying the list error", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("stop/start ran (calls %v)", *calls)
			}
		})
	}
}

// The restart command with no target pid (discovery saw no process) still
// refuses when a provider process is live: its stop would shut that one down.
func TestCmdRestartOnWindowsRefusesWithoutTargetPIDWhenOneRuns(t *testing.T) {
	calls := stubWindowsRestart(t, true, nil)
	listProviderProcessesFn = withProcesses(sharingProc)
	err := cmdRestartOnWindows(Provider{StateDir: t.TempDir()}, false, false)
	if err == nil || !strings.Contains(err.Error(), "pid 5151") {
		t.Fatalf("err = %v, want a refusal naming pid 5151", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("stop/start ran (calls %v)", *calls)
	}
}

func TestCmdRestartOnWindowsPassesFlagsThrough(t *testing.T) {
	stubWindowsRestart(t, true, nil)
	var gotForce, gotDry bool
	restartWindowsCmdFn = func(_ Provider, force, dryRun bool) error {
		gotForce, gotDry = force, dryRun
		return nil
	}
	if err := cmdRestartOnWindows(Provider{PID: 4242, StateDir: t.TempDir()}, true, true); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !gotForce || !gotDry {
		t.Fatalf("force=%v dryRun=%v, want both passed through", gotForce, gotDry)
	}
}
