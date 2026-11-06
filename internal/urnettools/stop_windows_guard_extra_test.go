//go:build windows

package urnettools

import (
	"errors"
	"strings"
	"testing"
)

func TestStopWindowsGuardCases(t *testing.T) {
	old := listProviderProcessesFn
	t.Cleanup(func() { listProviderProcessesFn = old })
	for _, tc := range []struct {
		name string
		list func() ([]providerProcess, error)
		want string
	}{
		{"list failure", func() ([]providerProcess, error) { return nil, errors.New("snapshot denied") }, "snapshot denied"},
		{"unknown owner", withProcesses(unknownProc), "pid 6262"},
		{"same account", withProcesses(sharingProc), "pid 5151"},
		{"other account", withProcesses(otherAcctPrc), ""},
		{"no processes", withProcesses(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listProviderProcessesFn = tc.list
			err := cmdStopWindows(Provider{StateDir: t.TempDir()}, false, false)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("stop: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was stopped") {
				t.Fatalf("stop = %v, want refusal mentioning %q", err, tc.want)
			}
		})
	}
}

func TestStopWindowsForceDoesNotListProcesses(t *testing.T) {
	old := listProviderProcessesFn
	t.Cleanup(func() { listProviderProcessesFn = old })
	listProviderProcessesFn = func() ([]providerProcess, error) { t.Fatal("force unexpectedly listed processes"); return nil, nil }
	err := cmdStopWindows(Provider{StateDir: t.TempDir()}, true, false)
	if err == nil || !strings.Contains(err.Error(), "no PID available") {
		t.Fatalf("force stop = %v, want no PID refusal", err)
	}
}
