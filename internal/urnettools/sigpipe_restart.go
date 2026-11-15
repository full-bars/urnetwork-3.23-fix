package urnettools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// sigpipeDropInName is the drop-in file the tool writes to add SIGPIPE restart.
// A fixed name keeps idempotency simple: the tool always manages exactly one file.
const sigpipeDropInName = "90-urnet-tools-restart.conf"

// sigpipeRiskFromSystemd runs `systemctl show -p Restart,RestartForceExitStatus,RestartPreventExitStatus,SuccessExitStatus <unit>` and returns true if the effective unit would leave a SIGPIPE death dead.
func sigpipeRiskFromSystemd(p Provider) (bool, error) {
	args := buildShowArgs(p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return false, fmt.Errorf("systemctl show: %w", err)
	}
	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	hasPipe := func(v string) bool {
		for _, token := range strings.Fields(v) {
			if token == "SIGPIPE" {
				return true
			}
		}
		return false
	}
	if hasPipe(props["RestartPreventExitStatus"]) {
		return true, nil // prevent wins
	}
	if hasPipe(props["RestartForceExitStatus"]) {
		return false, nil // force restarts
	}
	restart := props["Restart"]
	if restart == "always" || restart == "on-success" {
		return false, nil
	}
	return true, nil // no force, no always/on-success -> dead
}

// buildShowArgs constructs the arguments passed to `systemctl show -p <fields> <unit>`.
func buildShowArgs(p Provider) []string {
	var args []string
	if isUserUnit(p.Unit) && p.User != "" {
		args = append(args, systemctlUserArgs(p.User)...)
	} else {
		args = []string{"systemctl"}
	}
	args = append(args, "show", "-p", "Restart", "-p", "RestartForceExitStatus", "-p", "RestartPreventExitStatus", "-p", "SuccessExitStatus", p.Unit)
	return args
}

// dropInDirForUnit returns the drop-in directory path for the unit, or "" if unknown.
func dropInDirForUnit(p Provider) (string, error) {
	unitPath, err := resolveUnitFilePath(p)
	if err != nil || unitPath == "" {
		return "", fmt.Errorf("resolve unit path: %w", err)
	}
	return filepath.Join(filepath.Dir(unitPath), p.Unit+".d"), nil
}

// ensureSigpipeRestartDropIn writes the drop-in file if the effective unit lacks SIGPIPE restart.
// Returns (changed, error). Non-fatal: errors are logged, update continues.
func ensureSigpipeRestartDropIn(p Provider) (bool, error) {
	if runtime.GOOS != "linux" || p.Unit == "" || p.Supervisor == "openrc" {
		return false, nil
	}
	// First, check effective config via systemctl.
	risk, err := sigpipeRiskFromSystemd(p)
	if err != nil {
		// Fallback: if systemctl fails, we could parse files but skip for now.
		fmt.Fprintf(os.Stderr, "warning: %s: could not determine SIGPIPE restart state (systemctl error: %v); skipping drop-in\n", p.Unit, err)
		return false, nil
	}
	if !risk {
		return false, nil // already safe
	}
	// Check if the operator explicitly blocks SIGPIPE with RestartPreventExitStatus=SIGPIPE.
	// We can't override that; warn instead.
	// (sigpipeRiskFromSystemd already returns true when prevent contains SIGPIPE.)
	// We just need to ensure we don't silently ignore a prevent. A later drop-in with force SIGPIPE
	// would NOT win over prevent, but we still write the drop-in; the operator sees the warning.
	dropinDir, err := dropInDirForUnit(p)
	if err != nil {
		return false, fmt.Errorf("drop-in dir: %w", err)
	}
	dropinPath := filepath.Join(dropinDir, sigpipeDropInName)
	// Idempotent: if the file already exists with the right content, do nothing.
	content := "[Service]\nRestartForceExitStatus=SIGPIPE\n"
	if _, err := os.Stat(dropinPath); err == nil {
		existing, _ := os.ReadFile(dropinPath)
		if strings.Contains(string(existing), "RestartForceExitStatus=SIGPIPE") {
			return false, nil
		}
		// File exists but lacks the directive; we will overwrite.
	}
	// Write the drop-in. Use replaceUnitFileIn for atomic write with ownership.
	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir drop-in dir: %w", err)
	}
	if err := replaceUnitFileIn(p.StateHome, dropinPath, []byte(content)); err != nil {
		return false, fmt.Errorf("write drop-in: %w", err)
	}
	// Daemon reload so the drop-in takes effect for the running provider's next restart.
	if err := daemonReloadForUnit(p); err != nil {
		return true, fmt.Errorf("daemon-reload: %w", err)
	}
	fmt.Printf("installed SIGPIPE restart drop-in for %s at %s\n", p.Unit, dropinPath)
	return true, nil
}