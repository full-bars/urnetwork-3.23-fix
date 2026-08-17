package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	if dryRun {
		fmt.Printf("[dry-run] would %s auto-start for %s\n", mode, providerLabel(p))
		return nil
	}

	return setAutoStart(p, mode == "on")
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
	label := autoUpdateLabel(p)
	if dryRun {
		fmt.Printf("[dry-run] would set auto-update %s for %s (%s)\n", interval, providerLabel(p), label)
		return nil
	}
	return setAutoUpdateSchedule(p, label, interval)
}

// autoUpdateLabel returns a stable identifier for the auto-update scheduling
// object (systemd timer on Linux, scheduled task on Windows). Platform-neutral
// so dry-run output is uniform; the platform implementation derives its own
// concrete name from it.
func autoUpdateLabel(p Provider) string {
	if p.Unit != "" {
		return strings.TrimSuffix(p.Unit, ".service") + "-update"
	}
	return "urnetwork-update"
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
	// Clean up platform lifecycle artifacts: on Unix the auto-update timer,
	// on Windows the scheduled tasks (heavyweight review S7).
	cleanupLifecycle(p)
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
	if path == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	if cleaned == "" || cleaned == "." {
		return false
	}
	if runtime.GOOS == "windows" {
		// Windows roots are drive paths (C:\, \\server\share). Reject
		// volume roots and UNC roots; accept any deeper absolute path.
		vol := filepath.VolumeName(cleaned)
		if vol != "" && (len(cleaned) == len(vol)+1 || cleaned == vol) { // "C:\" or "\\server\share"
			return false
		}
		if cleaned == `\\` || cleaned == `//` {
			return false
		}
		// Reject a bare drive-relative or relative path.
		if !filepath.IsAbs(cleaned) {
			return false
		}
		return true
	}
	// Unix: require an absolute path that is not the root.
	return strings.HasPrefix(path, "/") && cleaned != "/"
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

// writeTimerUnitAtomic writes a timer unit file via temp+rename so a crash
// never leaves a half-written unit. Platform-neutral (pure file I/O), shared
// by the platform lifecycle implementations and the cross-platform tests.
func writeTimerUnitAtomic(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
