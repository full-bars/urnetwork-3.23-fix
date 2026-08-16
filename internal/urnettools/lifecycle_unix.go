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
		return exec.Command("systemctl", "--user", "-M", p.User+"@", action, p.Unit).Run()
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
		if isUserUnit(timer) && p.User != "" {
			return exec.Command("systemctl", "--user", "-M", p.User+"@", "disable", "--now", timer).Run()
		}
		return exec.Command("systemctl", "disable", "--now", timer).Run()
	case "daily":
		return writeTimerCalendar(timer, p, "daily")
	case "weekly":
		return writeTimerCalendar(timer, p, "Sun *-*-* 00:00:00 UTC")
	case "monthly":
		return writeTimerCalendar(timer, p, "monthly")
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// writeTimerCalendar rewrites a timer unit's OnCalendar line and reloads.
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
	if err != nil {
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
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return err
	}
	if isUserUnit(timer) && p.User != "" {
		_ = exec.Command("systemctl", "--user", "-M", p.User+"@", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "-M", p.User+"@", "enable", "--now", timer).Run()
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
		_ = exec.Command("systemctl", "--user", "-M", p.User+"@", "disable", "--now", timer).Run()
		return
	}
	_ = exec.Command("systemctl", "disable", "--now", timer).Run()
}
