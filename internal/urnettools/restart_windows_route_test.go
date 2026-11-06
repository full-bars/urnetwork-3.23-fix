package urnettools

import (
	"errors"
	"strings"
	"testing"
)

func stubWindowsRestart(t *testing.T, alive bool, restartErr error) *[]int {
	t.Helper()
	origAlive, origRestart := restartWindowsAliveFn, restartProviderWindowsFn
	t.Cleanup(func() { restartWindowsAliveFn, restartProviderWindowsFn = origAlive, origRestart })
	var calls []int
	restartWindowsAliveFn = func(int) bool { return alive }
	restartProviderWindowsFn = func(p Provider) error {
		calls = append(calls, p.PID)
		return restartErr
	}
	return &calls
}

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
