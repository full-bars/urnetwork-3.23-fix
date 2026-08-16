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
// The legacy Startup-folder .lnk is removed in BOTH directions so an
// upgrade never auto-starts twice and auto-start off actually stops it
// (heavyweight review S6).
func setAutoStart(p Provider, on bool) error {
	taskName := autoStartTaskName(p)
	if on {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no binary path", providerLabel(p))
		}
		// The provider starts with the "provide" subcommand.
		tr := fmt.Sprintf(`"%s" provide`, p.Binary)
		if err := runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "onlogon"); err != nil {
			return err
		}
		return removeLegacyStartupLnk()
	}
	_ = deleteTaskIfExists(taskName)
	return removeLegacyStartupLnk()
}

// autoStartTaskName derives a per-provider autostart task name so two
// providers do not overwrite each other's task (heavyweight review S8).
func autoStartTaskName(p Provider) string {
	label := autoUpdateLabel(p)
	if label == "" || label == "urnetwork-update" {
		return "urnetwork-autostart"
	}
	return label + "-autostart"
}

// removeLegacyStartupLnk removes the pre-Go-tool Startup-folder shortcut so
// the schtasks mechanism is the only autostart.
func removeLegacyStartupLnk() error {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil
	}
	lnk := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "urnetwork.lnk")
	if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// deleteTaskIfExists removes a scheduled task if it exists. schtasks
// /delete /f on a MISSING task errors (0x80070002), so check first; a
// missing task is a clean no-op (DeepSeek SF4).
func deleteTaskIfExists(taskName string) error {
	if err := runSchtasks("/query", "/tn", taskName); err != nil {
		// Not found = fine (no-op).
		return nil
	}
	return runSchtasks("/delete", "/f", "/tn", taskName)
}

// setAutoUpdateSchedule manages the auto-update scheduled task. label is the
// platform-neutral identifier; the concrete task name is fixed on Windows.
// Weekly is the default posture and maps to midnight Sunday.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	// Per-provider task name so two providers do not overwrite each other's
	// update task (heavyweight review S8). label is "<unit>-update" on
	// systemd-derived providers, else the fixed default.
	taskName := label
	if taskName == "" {
		taskName = "urnetwork-update"
	}
	switch interval {
	case "off":
		return deleteTaskIfExists(taskName)
	case "daily":
		// -f: the task runs outside a shell with no TTY, so the update
		// confirm prompt would fail on EOF; force skips it.
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "daily", "/st", "00:00")
	case "weekly":
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "weekly", "/d", "SUN", "/st", "00:00")
	case "monthly":
		// Explicit /d 1 so "monthly" means the 1st, not schtasks' silent
		// day-1 default.
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "monthly", "/d", "1", "/st", "00:00")
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
// with the captured output. The subcommand MUST carry the leading slash
// (schtasks /create, /delete, /query) — `schtasks create` is invalid
// (caught by live Windows test 2026-08-16).
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
