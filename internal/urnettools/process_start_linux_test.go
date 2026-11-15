//go:build linux

package urnettools

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessStartedAfter(t *testing.T) {
	// Need a real process that started AFTER a known swap time.
	// Spawn a short-lived sleep process, record start after it begins.
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Give it a moment to fully start.
	time.Sleep(200 * time.Millisecond)
	swap := time.Now().Add(-1 * time.Second) // swap happened 1s ago
	after, err := processStartedAfter(pid, swap)
	if err != nil {
		cmd.Process.Kill()
		t.Fatalf("processStartedAfter failed: %v", err)
	}
	if !after {
		cmd.Process.Kill()
		t.Errorf("expected sleep process (pid %d) to have started after swap; got false", pid)
	}
	cmd.Process.Kill()

	// Future swap time: should NOT be after
	pidSelf := os.Getpid()
	swapFuture := time.Now().Add(10 * time.Second)
	afterFuture, err := processStartedAfter(pidSelf, swapFuture)
	if err != nil {
		t.Fatalf("processStartedAfter future swap failed: %v", err)
	}
	if afterFuture {
		t.Errorf("expected self process NOT to have started after future swap; got true")
	}

	// Invalid PID
	_, err = processStartedAfter(0, time.Now())
	if err == nil {
		t.Errorf("expected error for pid=0")
	}
	_, err = processStartedAfter(-1, time.Now())
	if err == nil {
		t.Errorf("expected error for pid=-1")
	}
	// Nonexistent PID
	_, err = processStartedAfter(999999999, time.Now())
	if err == nil {
		t.Errorf("expected error for nonexistent pid")
	}
}
