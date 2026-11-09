package urnettools

import (
	"fmt"
	"strings"
	"testing"
)

func TestUpdateVerification_HotSwapObservations(t *testing.T) {
	for _, tc := range []struct {
		name          string
		entry         bool
		ready         bool
		imageReadable bool
		socketOK      bool
		want          string
	}{
		{name: "no entry", want: "no matching provider entry was discovered"},
		{name: "not ready", entry: true, want: "matching provider entry was found but was not ready"},
		{name: "different image version", entry: true, ready: true, imageReadable: true, want: `running image reported version "old"`},
		{name: "different socket version", entry: true, ready: true, socketOK: true, want: `control socket reported version "old"`},
		{name: "no socket answer", entry: true, ready: true, want: "no verified socket answer from that pid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVerifyLoop(t)
			p := testProvider("/state", 100)
			verifyDiscoverFn = func() []Provider {
				if !tc.entry {
					return nil
				}
				return []Provider{{StateDir: p.StateDir, PID: 200, BinaryDeleted: !tc.ready}}
			}
			verifyPidIsAliveFn = func(int) bool { return false }
			verifyRunningImageHandleFn = func(int) (string, error) {
				if tc.imageReadable {
					return "/image", nil
				}
				return "", fmt.Errorf("unreadable")
			}
			verifyProviderVersionFn = func(string) string { return "old" }
			verifySocketVersionFn = func(p Provider, pid int) (string, bool) {
				if pid != p.PID {
					t.Errorf("pid %d != %d", pid, p.PID)
				}
				return "old", tc.socketOK
			}
			var err error
			out := captureStdout(t, func() { err = verifyRestartLoop(p, testConfig("new"), true, "") })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(out, "failed to take over") {
				t.Fatalf("unproven output: %s", out)
			}
		})
	}
}
