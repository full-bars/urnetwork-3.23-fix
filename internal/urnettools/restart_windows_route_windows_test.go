//go:build windows

package urnettools

import (
	"strings"
	"testing"
)

// restartProvider (the update and tune restart) must take the Windows
// stop/start route; the systemd path cannot restart a unit-less provider.
func TestRestartProviderRoutesWindowsToStopStart(t *testing.T) {
	calls := stubWindowsRestart(t, true, nil)
	if err := restartProvider(Provider{User: "u", PID: 4242, StateDir: t.TempDir()}); err != nil {
		if strings.Contains(err.Error(), "systemd") {
			t.Fatalf("restartProvider took the systemd path on Windows: %v", err)
		}
		t.Fatalf("restartProvider: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("Windows stop/start calls = %v, want 1", *calls)
	}
}
