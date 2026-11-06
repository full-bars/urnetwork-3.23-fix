//go:build windows

package urnettools

import (
	"strings"
	"testing"
)

func TestStopWindowsRefusesSharedProfile(t *testing.T) {
	p := Provider{User: currentUserName(), StateDir: t.TempDir()}
	old := listProviderProcessesFn
	t.Cleanup(func() { listProviderProcessesFn = old })
	listProviderProcessesFn = withProcesses(sharingProc)
	err := cmdStopWindows(p, false, false)
	if err == nil || !strings.Contains(err.Error(), "pid 5151") {
		t.Fatalf("stop error = %v; want refusal naming the other process", err)
	}
}
