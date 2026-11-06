//go:build windows

package urnettools

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// cmdStopWindows stops the provider on Windows by sending a shutdown command
// over the control socket, then waiting for the process to exit. Falls back
// to TerminateProcess if the graceful shutdown does not complete within 15s.
func cmdStopWindows(p Provider, force, dryRun bool) error {
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")

	// When force==true, bypass graceful shutdown and go straight to kill.
	if !force {
		// Attempt a graceful shutdown via the control socket.
		fmt.Println("sending shutdown command...")
		resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "shutdown"})
		if err != nil {
			if isSocketUnavailable(err) {
				// Socket is dead — but if the PID is alive, fall
				// through to the PID wait/terminate path below.
				if p.PID <= 0 {
					fmt.Printf("provider %s is not running (control socket unreachable)\n", providerLabel(p))
					return nil
				}
				fmt.Printf("warning: control socket unreachable, falling back to process kill\n")
			} else {
				fmt.Printf("warning: shutdown command failed: %v\n", err)
			}
		} else if !resp.OK {
			fmt.Printf("warning: shutdown response: %s\n", resp.Error)
		}
	} else {
		fmt.Println("force stop: skipping graceful shutdown")
		// Force mode: kill the process immediately without waiting.
		if p.PID <= 0 {
			return fmt.Errorf("cannot force-stop %s: no PID available", providerLabel(p))
		}
		if err := terminateProcess(p.PID); err != nil {
			return fmt.Errorf("terminate process %d: %w", p.PID, err)
		}
		cleanupStaleSocket(sockPath)
		fmt.Printf("forcefully terminated %s\n", providerLabel(p))
		return nil
	}

	// Wait up to 15 seconds for the process to exit.
	if p.PID > 0 {
		deadline := time.After(15 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline:
				fmt.Println("provider did not exit within 15s, terminating...")
				if err := terminateProcess(p.PID); err != nil {
					return fmt.Errorf("terminate process %d: %w", p.PID, err)
				}
				cleanupStaleSocket(sockPath)
				fmt.Printf("forcefully terminated %s\n", providerLabel(p))
				return nil
			case <-ticker.C:
				if !pidIsAlive(p.PID) {
					fmt.Printf("stopped %s\n", providerLabel(p))
					return nil
				}
			}
		}
	}

	// If we had no PID, poll the control socket to confirm shutdown.
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return fmt.Errorf("provider %s did not shut down within 10s (no PID to kill)", providerLabel(p))
		case <-ticker.C:
			if !controlSocketReachable(p) {
				fmt.Printf("stopped %s\n", providerLabel(p))
				return nil
			}
		}
	}
}

// terminateProcess kills a Windows process by PID using TerminateProcess,
// then waits for the process object to signal so callers don't race with
// port/socket release.
func terminateProcess(pid int) error {
	const processTerminate = 0x0001

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procOpen := kernel32.NewProc("OpenProcess")
	procTerminate := kernel32.NewProc("TerminateProcess")

	handle, _, _ := procOpen.Call(
		uintptr(processTerminate|syscall.SYNCHRONIZE),
		0, // bInheritHandle = FALSE
		uintptr(pid),
	)
	if handle == 0 {
		return fmt.Errorf("OpenProcess failed for pid %d", pid)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	ret, _, _ := procTerminate.Call(handle, 1)
	if ret == 0 {
		return fmt.Errorf("TerminateProcess failed for pid %d", pid)
	}
	// Wait for the process to actually exit so subsequent starts don't
	// race with releasing sockets, ports, and file locks.
	syscall.WaitForSingleObject(syscall.Handle(handle), 5000)
	return nil
}

// cleanupStaleSocket removes a leftover provider.sock file after a forceful
// termination or hard crash; leaving it behind would block the next
// net.Listen on Windows. It first checks that nothing listens on it: a
// second provider process can share the default state dir, and deleting
// its live socket would cut it off from every control command.
func cleanupStaleSocket(path string) {
	if _, err := os.Stat(path); err != nil {
		return // already gone
	}
	if conn, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		conn.Close()
		fmt.Printf("note: left %s in place: another provider process is still listening on it; stop that one too before restarting\n", path)
		return
	}
	if err := os.Remove(path); err != nil {
		fmt.Printf("warning: could not remove stale socket %s: %v\n", path, err)
	}
}
