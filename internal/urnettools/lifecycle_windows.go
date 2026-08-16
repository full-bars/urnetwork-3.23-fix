//go:build windows

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Windows lifecycle uses Task Scheduler (schtasks.exe) instead of systemd.
// The provider runs as a user-level process on Windows, so tasks are created
// without /ru (current user, runs only while logged on — no stored
// credentials, matching the provider's user-session model).

// setAutoStart enables or disables login auto-start for the provider by
// registering (or removing) a Task Scheduler task that runs on logon.
func setAutoStart(p Provider, on bool) error {
	if on {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no binary path", providerLabel(p))
		}
		// The provider starts with the "provide" subcommand.
		tr := fmt.Sprintf(`"%s" provide`, p.Binary)
		return runSchtasks("create", "/f", "/tn", "urnetwork-autostart", "/tr", tr, "/sc", "onlogon")
	}
	return deleteTaskIfExists("urnetwork-autostart")
}

// deleteTaskIfExists removes a scheduled task if it exists. schtasks
// /delete /f on a MISSING task errors (0x80070002), so check first; a
// missing task is a clean no-op (DeepSeek SF4).
func deleteTaskIfExists(taskName string) error {
	if err := runSchtasks("query", "/tn", taskName); err != nil {
		// Not found = fine (no-op).
		return nil
	}
	return runSchtasks("delete", "/f", "/tn", taskName)
}

// setAutoUpdateSchedule manages the auto-update scheduled task. label is the
// platform-neutral identifier; the concrete task name is fixed on Windows.
// Weekly is the default posture and maps to midnight Sunday.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	const taskName = "urnetwork-update"
	switch interval {
	case "off":
		return deleteTaskIfExists(taskName)
	case "daily":
		// -f: the task runs outside a shell with no TTY, so the update
		// confirm prompt would fail on EOF; force skips it (DeepSeek SF5).
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("create", "/f", "/tn", taskName, "/tr", tr, "/sc", "daily", "/st", "00:00")
	case "weekly":
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("create", "/f", "/tn", taskName, "/tr", tr, "/sc", "weekly", "/d", "SUN", "/st", "00:00")
	case "monthly":
		// Explicit /d 1 so "monthly" means the 1st, not schtasks' silent
		// day-1 default (DeepSeek NICE 8).
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("create", "/f", "/tn", taskName, "/tr", tr, "/sc", "monthly", "/d", "1", "/st", "00:00")
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// toolExePath returns the running tool's absolute path, which the scheduled
// task uses as its executable (the task runs outside a shell, so PATH is not
// available).
func toolExePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Clean(exe)
	}
	return "urnet-tools.exe"
}

// runSchtasks executes schtasks.exe with the given args and wraps failures
// with the captured output.
func runSchtasks(subcommand string, args ...string) error {
	full := append([]string{subcommand}, args...)
	out, err := exec.Command("schtasks", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %w (%s)", subcommand, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cleanupLifecycle removes the Windows scheduled tasks on uninstall.
func cleanupLifecycle(p Provider) {
	_ = deleteTaskIfExists("urnetwork-update")
	_ = deleteTaskIfExists("urnetwork-autostart")
}
