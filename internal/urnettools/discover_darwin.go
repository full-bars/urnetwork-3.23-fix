//go:build darwin

package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// discoverProcesses on macOS enumerates running provider processes via pgrep
// (there is no /proc) and falls back to the standard install location so
// lifecycle commands have a target on a stopped install. There are no
// systemd units; Unit stays empty.
func discoverProcesses() []Provider {
	var out []Provider
	// pgrep -x matches the exact process name. Exit 1 (no match) is an
	// empty result, not an error.
	for _, name := range []string{"urnetwork", "urnetwork.exe"} {
		cmd := exec.Command("pgrep", "-x", name)
		raw, err := cmd.Output()
		if err != nil {
			continue // no match or pgrep missing
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			exe := resolveMacProcessExe(pid, name)
			out = append(out, Provider{
				User:     os.Getenv("USER"),
				StateDir: macStateDir(),
				Binary:   exe,
				PID:      pid,
				Running:  true,
			})
		}
	}
	if len(out) == 0 {
		if exe := installedMacProvider(); exe != "" {
			out = append(out, Provider{
				User:     os.Getenv("USER"),
				StateDir: macStateDir(),
				Binary:   exe,
				Running:  false,
			})
		}
	}
	return out
}

// resolveMacProcessExe returns the executable path of a running process,
// falling back to the image name. macOS has no /proc/<pid>/exe; use
// `lsof -p <pid> | awk '$1=="cwd"'` is overkill — ps -o comm is enough.
func resolveMacProcessExe(pid int, imageName string) string {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=")
	if raw, err := cmd.Output(); err == nil {
		p := strings.TrimSpace(string(raw))
		if p != "" {
			return p
		}
	}
	return imageName
}

// installedMacProvider locates the provider in the standard macOS install
// location (~/.local/share/urnetwork-provider/bin/urnetwork, matching the
// Linux shell toolchain layout on mac).
func installedMacProvider() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork"),
		filepath.Join(home, ".urnetwork", "urnetwork"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// macStateDir is the provider state dir on macOS.
func macStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".urnetwork")
}

// discoverStopped on macOS is a no-op: launchd has no unit enumeration here
// (the process scan already falls back to the standard install location for
// stopped installs), so there are no additional providers to add.
func discoverStopped(running []Provider) []Provider {
	return nil
}
