package urnettools

import (
	"errors"
	"strings"
	"testing"
)

// stubWindowsStart stubs the Windows start seams so no test launches a
// process or runs a scheduled task. It returns the launch count.
func stubWindowsStart(t *testing.T, procs func() ([]providerProcess, error), socketUp bool) *int {
	t.Helper()
	origStart, origList, origSocket := startWindowsFn, listProviderProcessesFn, startSocketReachableFn
	t.Cleanup(func() {
		startWindowsFn, listProviderProcessesFn, startSocketReachableFn = origStart, origList, origSocket
	})
	launches := 0
	startWindowsFn = func(Provider, bool, bool) error {
		launches++
		return nil
	}
	listProviderProcessesFn = procs
	startSocketReachableFn = func(Provider) bool { return socketUp }
	return &launches
}

// Windows start used to launch the provider (or run its logon task) with no
// check, so a second start ran a second copy on the same profile. Like the
// FreeBSD rc script and systemd, an already-running provider is reported
// and start exits 0 without launching anything.
func TestStartProviderOnWindowsSkipsWhenAlreadyRunning(t *testing.T) {
	cases := []struct {
		name     string
		procs    func() ([]providerProcess, error)
		socketUp bool
		want     string
	}{
		{"same user process", withProcesses(targetProc), false, "pid 4242"},
		{"unknown owner counts", withProcesses(unknownProc), false, "pid 6262"},
		{"two processes listed", withProcesses(targetProc, sharingProc), false, "pid 5151"},
		{"socket answers, no process seen", withProcesses(), true, "provider.sock"},
		{"list fails, socket answers", func() ([]providerProcess, error) { return nil, errors.New("snapshot denied") }, true, "provider.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launches := stubWindowsStart(t, tc.procs, tc.socketUp)
			var err error
			out := captureStdout(t, func() {
				err = startProviderOnWindows(Provider{StateDir: t.TempDir()}, false, false)
			})
			if err != nil {
				t.Fatalf("err = %v, want nil (already running is not a failure)", err)
			}
			if *launches != 0 {
				t.Fatalf("launched %d time(s) while a provider runs", *launches)
			}
			for _, want := range []string{"already running", "not starting a second copy", tc.want} {
				if !strings.Contains(out, want) {
					t.Errorf("output %q, want it to mention %q", out, want)
				}
			}
		})
	}
}

func TestStartProviderOnWindowsLaunchesWhenNothingRuns(t *testing.T) {
	cases := []struct {
		name  string
		procs func() ([]providerProcess, error)
	}{
		{"empty list", withProcesses()},
		{"only another account's provider", withProcesses(otherAcctPrc)},
		// A list failure must not block start: the socket check still runs
		// and a provider that answers there is caught above.
		{"list fails, socket silent", func() ([]providerProcess, error) { return nil, errors.New("snapshot denied") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launches := stubWindowsStart(t, tc.procs, false)
			if err := startProviderOnWindows(Provider{StateDir: t.TempDir()}, false, false); err != nil {
				t.Fatalf("start: %v", err)
			}
			if *launches != 1 {
				t.Fatalf("launches = %d, want 1", *launches)
			}
		})
	}
}

func TestStartProviderOnWindowsPropagatesLaunchFailure(t *testing.T) {
	stubWindowsStart(t, withProcesses(), false)
	startWindowsFn = func(Provider, bool, bool) error { return errors.New("start provider: boom") }
	if err := startProviderOnWindows(Provider{StateDir: t.TempDir()}, false, false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the launch failure", err)
	}
}
