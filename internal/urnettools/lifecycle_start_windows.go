//go:build windows

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// cmdStartWindows starts the provider on Windows.
//
// If an auto-start scheduled task already exists, it runs it via schtasks.
// Otherwise the provider is launched detached and hidden so the CLI can
// return immediately.
func cmdStartWindows(p Provider, force, dryRun bool) error {
	taskName := autoStartTaskName(p)

	// Check whether the auto-start task exists by querying schtasks.
	// If it does, just run it — the task already knows the binary path
	// and arguments (created by setAutoStart).
	if err := runSchtasks("/query", "/tn", taskName); err == nil {
		// Task exists — run it.
		fmt.Printf("running auto-start task %s for %s\n", taskName, providerLabel(p))
		return runSchtasks("/run", "/tn", taskName)
	}

	// No auto-start task — launch the provider detached and hidden.
	// Do NOT create a logon task here; that is setAutoStart's job.
	if p.Binary == "" {
		return fmt.Errorf("provider %s has no binary path", providerLabel(p))
	}

	fmt.Printf("launching %s directly\n", providerLabel(p))

	const (
		detachedProcess        = 0x00000008
		createNewProcessGrp    = 0x00000200
		createNoWindow         = 0x08000000
		createBreakawayFromJob = 0x01000000
	)

	cmd := exec.Command(p.Binary, "provide")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// CREATE_BREAKAWAY_FROM_JOB prevents the provider from being
		// killed when the CLI exits under a Windows Job Object (the
		// default in PowerShell 7, Windows Terminal, VS Code, and CI
		// runners). Without it the parent's job object terminates the
		// provider on CLI exit even with DETACHED_PROCESS.
		CreationFlags: detachedProcess | createNewProcessGrp | createNoWindow | createBreakawayFromJob,
	}
	// Ensure the provider's state directory exists before launching.
	// On a fresh install or clean profile, %USERPROFILE%\.urnetwork does
	// not exist yet; setting cmd.Dir to a nonexistent directory causes
	// Windows CreateProcess to fail immediately with Win32 error 267
	// (ERROR_DIRECTORY_NAME / "The directory name is invalid.").
	if p.StateDir != "" {
		if err := os.MkdirAll(p.StateDir, 0755); err != nil {
			return fmt.Errorf("start provider: create state dir %s: %w", p.StateDir, err)
		}
	}
	cmd.Dir = p.StateDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start provider: %w", err)
	}

	// Release the process so the CLI can exit. We intentionally do not
	// wait for it — the provider runs as a background daemon.
	_ = cmd.Process.Release()
	return nil
}
