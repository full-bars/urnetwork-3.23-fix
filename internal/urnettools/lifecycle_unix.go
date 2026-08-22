//go:build linux

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// setAutoStart enables or disables login auto-start for the provider's
// owning systemd unit. On Linux/macOS the provider runs as a systemd (or
// launchd) unit, so this is the systemctl enable/disable path.
func setAutoStart(p Provider, on bool) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}
	action := "disable"
	if on {
		action = "enable"
	}
	if isUserUnit(p.Unit) && p.User != "" {
		args := append(systemctlUserArgs(p.User), action, p.Unit)
		return exec.Command("systemctl", args...).Run()
	}
	return exec.Command("systemctl", action, p.Unit).Run()
}

// setAutoUpdateSchedule manages the auto-update systemd timer. label is the
// platform-neutral identifier ("<unit>-update" or "urnetwork-update"); the
// concrete timer unit name is derived from the provider unit.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}
	timer := strings.TrimSuffix(p.Unit, ".service") + "-update.timer"
	switch interval {
	case "off":
		// The deterministic part is the FILE removal — a disabled-but-present
		// timer file would keep firing. A failed disable (e.g. no session bus
		// on a CI runner) is logged, not propagated: the caller cares that the
		// timer is gone, and it is. The systemd manager state resolves on the
		// next daemon-reload even when disable could not reach it now.
		if isUserUnit(timer) && p.User != "" {
			args := append(systemctlUserArgs(p.User), "disable", "--now", timer)
			if err := exec.Command("systemctl", args...).Run(); err != nil {
				fmt.Fprintf(os.Stderr, "auto-update off: warning: disable %s: %v (timer file still removed)\n", timer, err)
			}
		} else if err := exec.Command("systemctl", "disable", "--now", timer).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "auto-update off: warning: disable %s: %v (timer file still removed)\n", timer, err)
		}
		return removeTimerFile(p, timer)
	case "daily":
		return writeTimerCalendar(timer, p, "daily")
	case "weekly":
		return writeTimerCalendar(timer, p, "Sun *-*-* 00:00:00 UTC")
	case "monthly":
		return writeTimerCalendar(timer, p, "monthly")
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// removeTimerFile deletes the timer unit file for a provider, mirroring the
// shell wrapper's `rm -f` on auto-update off (Provider_Install_Linux.sh).
// A missing file is not an error.
func removeTimerFile(p Provider, timer string) error {
	var path string
	if isUserUnit(timer) && p.User != "" {
		home := homeForUser(p.User)
		if home == "" {
			return fmt.Errorf("cannot resolve home for user %s", p.User)
		}
		path = filepath.Join(home, ".config/systemd/user", timer)
	} else {
		path = "/etc/systemd/system/" + timer
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeTimerCalendar rewrites a timer unit's OnCalendar line and reloads. If
// the timer file does not exist yet (fresh install — the Go tool, unlike the
// shell installer, may be the first thing to set auto-update), it is CREATED
// with the same [Unit]/[Timer]/[Install] shape the shell wrapper writes at
// install time (Provider_Install_Linux.sh:685).
func writeTimerCalendar(timer string, p Provider, calendar string) error {
	// Locate the timer unit file (system or user).
	var path string
	if isUserUnit(timer) && p.User != "" {
		home := homeForUser(p.User)
		if home == "" {
			return fmt.Errorf("cannot resolve home for user %s", p.User)
		}
		path = filepath.Join(home, ".config/systemd/user", timer)
	} else {
		path = "/etc/systemd/system/" + timer
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Create the timer from scratch, mirroring the installer's
		// template. Write to a temp file and rename so a crash mid-write
		// never leaves a half-written unit (the shared progress.md
		// atomicity rule applied to unit files too).
		tmpl := fmt.Sprintf("[Unit]\nDescription=Run URnetwork Update\n\n[Timer]\nOnCalendar=%s\nPersistent=true\n\n[Install]\nWantedBy=default.target\n", calendar)
		if err := writeTimerUnitAtomic(path, tmpl); err != nil {
			return fmt.Errorf("create timer %s: %w", timer, err)
		}
		return enableTimer(p, timer)
	} else if err != nil {
		return fmt.Errorf("read timer %s: %w", timer, err)
	}
	lines := strings.Split(string(data), "\n")
	var out []string
	replaced := false
	for _, line := range lines {
		if strings.HasPrefix(line, "OnCalendar=") {
			out = append(out, "OnCalendar="+calendar)
			replaced = true
		} else {
			out = append(out, line)
		}
	}
	if !replaced {
		return fmt.Errorf("no OnCalendar line found in %s", path)
	}
	if err := writeTimerUnitAtomic(path, strings.Join(out, "\n")); err != nil {
		return err
	}
	return enableTimer(p, timer)
}

// enableTimer runs daemon-reload + enable --now for a timer unit, choosing
// the user or system manager from the provider's unit placement.
func enableTimer(p Provider, timer string) error {
	if isUserUnit(timer) && p.User != "" {
		args := append(systemctlUserArgs(p.User), "daemon-reload")
		_ = exec.Command("systemctl", args...).Run()
		args = append(systemctlUserArgs(p.User), "enable", "--now", timer)
		return exec.Command("systemctl", args...).Run()
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	return exec.Command("systemctl", "enable", "--now", timer).Run()
}

// cleanupLifecycle on Unix disables the auto-update timer. The unit disable
// in cmdUninstall handles the service, but the <unit>-update.timer would
// keep firing for a provider that is gone (heavyweight review S7).
func cleanupLifecycle(p Provider) {
	if p.Unit == "" {
		return
	}
	timer := strings.TrimSuffix(p.Unit, ".service") + "-update.timer"
	if isUserUnit(timer) && p.User != "" {
		args := append(systemctlUserArgs(p.User), "disable", "--now", timer)
		_ = exec.Command("systemctl", args...).Run()
		return
	}
	_ = exec.Command("systemctl", "disable", "--now", timer).Run()
}

// renderSystemctlStatus reproduces the pre-rewrite Linux `status` behavior:
// runs `systemctl status <unit>` (user or system scope) exactly as the old
// tool did (`systemctl --user status urnetwork.service`), giving the full
// systemd view (load, ActiveState, uptime, memory, tasks, unit path).
func init() {
	renderSystemctlStatus = renderSystemctlStatusLinux
}

func renderSystemctlStatusLinux(p Provider) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit (bare process)", providerLabel(p))
	}
	var args []string
	if p.User != "" {
		args = append(systemctlUserArgs(p.User), "status", p.Unit)
	} else {
		args = []string{"status", p.Unit}
	}
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
