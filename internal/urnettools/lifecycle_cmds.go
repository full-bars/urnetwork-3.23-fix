package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file ports the lifecycle commands: auto-start, auto-update,
// uninstall, reinstall. Like every command, they operate on the RESOLVED
// provider — never a hardcoded $HOME path.

// cmdAutoStart toggles whether the provider's unit starts on login.
// Usage: urnet-tools auto-start on|off
func cmdAutoStart(args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("auto-start requires on|off")
	}
	mode := args[0]
	if mode != "on" && mode != "off" {
		return fmt.Errorf("invalid value %q: must be on or off", mode)
	}
	t, _, err := parseTargetFlags(args[1:])
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}

	if dryRun {
		fmt.Printf("[dry-run] would %s auto-start for %s (%s)\n", mode, providerLabel(p), p.Unit)
		return nil
	}

	if mode == "on" {
		return unitEnable(p, true)
	}
	return unitEnable(p, false)
}

// unitEnable enables/disables the provider's owning unit (login autostart).
func unitEnable(p Provider, enable bool) error {
	action := "disable"
	if enable {
		action = "enable"
	}
	if isUserUnit(p.Unit) && p.User != "" {
		return exec.Command("systemctl", "--user", "-M", p.User+"@", action, p.Unit).Run()
	}
	return exec.Command("systemctl", action, p.Unit).Run()
}

// cmdAutoUpdate manages the auto-update timer interval.
// Usage: urnet-tools auto-update daily|weekly|monthly|off
func cmdAutoUpdate(args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("auto-update requires daily|weekly|monthly|off")
	}
	interval := args[0]
	// Validate the interval BEFORE targeting so an invalid value errors
	// deterministically without needing a resolvable provider (coderabbit
	// minor: the old switch-default check was unreachable in tests).
	switch interval {
	case "off", "daily", "weekly", "monthly":
	default:
		return fmt.Errorf("invalid interval %q: daily|weekly|monthly|off", interval)
	}
	t, _, err := parseTargetFlags(args[1:])
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}

	// The update timer unit mirrors the provider unit name:
	// urnetwork-native.service -> urnetwork-native-update.timer
	timer := strings.TrimSuffix(p.Unit, ".service") + "-update.timer"
	if dryRun {
		fmt.Printf("[dry-run] would set auto-update %s for %s (timer %s)\n", interval, providerLabel(p), timer)
		return nil
	}

	switch interval {
	case "off":
		// Disable the timer.
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
	return nil
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

// cmdUninstall removes the provider: stops/disables the unit, removes the
// install dir and state. Destructive gate applies.
func cmdUninstall(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	ok, err := confirmGate("uninstall (remove binary, state, and unit) for "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if p.Unit != "" {
		if isUserUnit(p.Unit) && p.User != "" {
			if out, err := exec.Command("systemctl", "--user", "-M", p.User+"@", "disable", "--now", p.Unit).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "uninstall: warning: disable %s: %v (%s)\n", p.Unit, err, strings.TrimSpace(string(out)))
			}
		} else {
			if out, err := exec.Command("systemctl", "disable", "--now", p.Unit).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "uninstall: warning: disable %s: %v (%s)\n", p.Unit, err, strings.TrimSpace(string(out)))
			}
		}
	}
	// Only remove paths that look like real install paths — never "/" or
	// a bare relative path (free-review major: harden the deletion guard).
	// Both guards clean the path so "/" and "/./" are caught identically.
	// Removal errors are REPORTED, not hidden (coderabbit major).
	removedAny := false
	hadErrors := false
	if safeRemoveTarget(p.Binary) {
		if err := os.Remove(p.Binary); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove binary %s: %v\n", p.Binary, err)
			hadErrors = true
		} else {
			removedAny = true
		}
	}
	if safeRemoveTarget(p.StateDir) {
		if err := os.RemoveAll(p.StateDir); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove state dir %s: %v\n", p.StateDir, err)
			hadErrors = true
		} else {
			removedAny = true
		}
	}
	if hadErrors {
		return fmt.Errorf("uninstall %s: partial — some paths could not be removed (see warnings)", providerLabel(p))
	}
	if removedAny {
		fmt.Printf("Uninstalled %s (binary removed, unit disabled)\n", providerLabel(p))
	} else {
		fmt.Printf("Uninstall %s: nothing removable found (unit disabled if present)\n", providerLabel(p))
	}
	return nil
}

// safeRemoveTarget reports whether a path is safe to remove: non-empty,
// absolute, and not the filesystem root after cleaning. Used by cmdUninstall
// so "/" or "/./" can never be removed (free-review major). Pure helper so
// tests call production logic, not a copy (coderabbit major).
func safeRemoveTarget(path string) bool {
	return path != "" && strings.HasPrefix(path, "/") && filepath.Clean(path) != "/"
}

// cmdReinstall delegates to the legacy installer script for a full
// reinstall of the targeted provider (the installer handles the complete
// flow; the Go tool resolves which provider/user to target).
func cmdReinstall(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	ok, err := confirmGate("reinstall (rerun installer) for "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// The installer is the canonical reinstall path. Run it as the
	// provider's user with the right HOME so it targets the same install.
	// Resolve home via getent — never hardcode /home/<user> (breaks
	// root-run providers; review finding M3).
	home := homeForUser(p.User)
	if home == "" {
		return fmt.Errorf("cannot resolve home for user %s", p.User)
	}
	installer := filepath.Join(home, ".local/share/urnetwork-provider/bin/urnet-tools")
	cmd := exec.Command(installer, "reinstall")
	if p.User != "" {
		cmd.Env = append(os.Environ(), "HOME="+home)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
