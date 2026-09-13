//go:build unix

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// containerUpdatePendingMarker tells the image's start script that the
// provider exited because its binary was replaced, so it relaunches the
// provider instead of treating the exit as a shutdown (see
// docker/scripts/start_*.sh).
const containerUpdatePendingMarker = "update-pending"

// Timings for restartContainerProvider, overridable in tests.
var (
	containerStopWait    = 15 * time.Second
	containerKillWait    = 5 * time.Second
	containerRespawnWait = 30 * time.Second
	containerDiscover    = Discover
)

// restartContainerProvider restarts a provider that runs under a container's
// start script, which is how an in-place update or a session load takes
// effect without systemd. It marks the exit as a binary update, stops the
// process (SIGTERM, then SIGKILL after containerStopWait), waits for the
// start script to launch the replacement, and then clears the marker so a
// later clean exit, such as `docker stop`, is not mistaken for another update.
func restartContainerProvider(p Provider) error {
	if p.PID <= 0 {
		return fmt.Errorf("no running provider process to restart in this container")
	}
	stateDir := p.StateDir
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve provider state dir: %w", err)
		}
		stateDir = filepath.Join(home, ".urnetwork")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}
	if err := writeStateFile(stateDir, containerUpdatePendingMarker, nil, 0o644); err != nil {
		return fmt.Errorf("write %s marker: %w", containerUpdatePendingMarker, err)
	}
	marker := filepath.Join(stateDir, containerUpdatePendingMarker)

	if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		os.Remove(marker)
		return fmt.Errorf("signal provider pid %d: %w", p.PID, err)
	}
	if !waitForProcessExit(p.PID, containerStopWait) {
		fmt.Printf("provider pid %d still running %s after SIGTERM; sending SIGKILL\n", p.PID, containerStopWait)
		_ = syscall.Kill(p.PID, syscall.SIGKILL)
		if !waitForProcessExit(p.PID, containerKillWait) {
			os.Remove(marker)
			return fmt.Errorf("provider pid %d survived SIGKILL", p.PID)
		}
	}
	fmt.Printf("stopped provider pid %d; waiting for the container's start script to relaunch it\n", p.PID)

	newPID, ok := waitForContainerRespawn(p, containerRespawnWait)
	// The start script removes the marker itself when it consumes a clean
	// exit; a SIGKILL exit leaves it behind, so always clear it here.
	os.Remove(marker)
	if !ok {
		return fmt.Errorf("provider did not relaunch within %s; check the container's start script output (docker logs)", containerRespawnWait)
	}
	fmt.Printf("provider relaunched (pid %d)\n", newPID)
	return nil
}

// waitForProcessExit polls until pid is gone or d elapses.
func waitForProcessExit(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !pidIsAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForContainerRespawn polls discovery for a provider on the same state
// dir with a different pid.
func waitForContainerRespawn(old Provider, d time.Duration) (int, bool) {
	deadline := time.Now().Add(d)
	for {
		for _, rp := range containerDiscover() {
			if rp.PID > 0 && rp.PID != old.PID && (old.StateDir == "" || rp.StateDir == old.StateDir) {
				return rp.PID, true
			}
		}
		if !time.Now().Before(deadline) {
			return 0, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
