//go:build freebsd

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FreeBSD lifecycle: an rc.d(8) service script plus a periodic auto-update
// driven from cron(8).
//
// rc.d is the analogue of a systemd service unit, and it is the right shape to
// target: `service urnetwork start|stop|status` is what a FreeBSD operator
// expects, and the rc subsystem keeps the enabled flag in /etc/rc.conf, which is
// what setAutoStart toggles through sysrc(8).
//
// The auto-update scheduler has no timer equivalent. systemd supplies
// OnCalendar timers; FreeBSD supplies cron, so a schedule is a crontab line.
// That is weaker in one specific way worth stating: cron has no missed-run
// catch-up, so a box that was down at the scheduled minute does not update on
// the next boot the way a systemd timer would have.

// bsdUpdateCronLine returns the crontab line for an auto-update interval.
//
// The command runs `urnet-tools update -f`, the same thing the systemd timer
// runs, and appends to a log so an unattended update that fails leaves a trail
// instead of vanishing. An unknown interval is rejected rather than defaulted:
// silently installing a daily schedule for someone who asked for weekly is worse
// than an error.
func bsdUpdateCronLine(tool, logDir, interval string) (string, error) {
	var spec string
	switch interval {
	case "daily":
		spec = "17 3 * * *"
	case "weekly":
		// Sunday 03:17.
		spec = "17 3 * * 0"
	case "monthly":
		// The 1st of the month at 03:17.
		spec = "17 3 1 * *"
	default:
		return "", fmt.Errorf("invalid interval %q", interval)
	}
	return fmt.Sprintf("%s %s update -f >> %s 2>&1",
		spec, shellQuote(tool), shellQuote(filepath.Join(logDir, "auto-update.log"))), nil
}

// shellQuote wraps s in single quotes for inclusion in a crontab line, which is
// executed by /bin/sh. A path containing a space or a quote must not break the
// schedule.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// setAutoStart enables or disables boot-time start by flipping the service's
// rc.conf enable flag with sysrc(8).
//
// sysrc is used rather than editing /etc/rc.conf directly: it rewrites the file
// atomically and preserves its formatting, which a hand-written redirect cannot
// guarantee on a file every other service on the box also depends on.
func setAutoStart(p Provider, on bool) error {
	service := bsdRcServiceName(p.Binary)
	value := "NO"
	if on {
		value = "YES"
	}
	if out, err := exec.Command("sysrc", service+"_enable="+value).CombinedOutput(); err != nil {
		return fmt.Errorf("auto-start %s: sysrc %s_enable=%s: %w (%s)",
			providerLabel(p), service, value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setAutoUpdateSchedule installs or removes the cron entry for auto-update.
//
// label is the platform-neutral identifier the caller computed. FreeBSD derives
// its own name from the service instead, matching how the Linux timer name is
// derived from the provider unit rather than from the label.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	if interval == "off" {
		return removeBSDUpdateCron(p)
	}
	line, err := bsdUpdateCronLine(toolExePath(), bsdLogDir(), interval)
	if err != nil {
		return err
	}
	return writeBSDUpdateCron(p, line)
}

// cleanupLifecycle removes the auto-update cron entry on uninstall. The rc.d
// script is removed by the uninstall command's own service handling.
func cleanupLifecycle(p Provider) {
	_ = removeBSDUpdateCron(p)
}

// bsdLogDir is where the unattended update log is written: the provider's state
// dir, which the invoking user owns. It falls back to /var/tmp only when the
// home cannot be resolved, since /var/tmp is world-writable.
func bsdLogDir() string {
	if home := homeForUser(currentUserName()); home != "" {
		return filepath.Join(home, ".urnetwork")
	}
	return "/var/tmp"
}

// writeBSDUpdateCron replaces this provider's auto-update line in the invoking
// user's crontab, leaving every other line untouched.
//
// The previous line for the same service is dropped first, so changing the
// interval never leaves two schedules running.
func writeBSDUpdateCron(p Provider, line string) error {
	service := bsdRcServiceName(p.Binary)
	existing, err := readUserCrontab()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(existing)+1)
	for _, l := range existing {
		if !matchBSDCronLine(l, service) {
			kept = append(kept, l)
		}
	}
	kept = append(kept, line+" "+bsdCronMarker+" "+service)
	return installUserCrontab(kept)
}

// removeBSDUpdateCron drops this provider's auto-update line. A crontab with no
// line of ours is left untouched rather than rewritten.
func removeBSDUpdateCron(p Provider) error {
	service := bsdRcServiceName(p.Binary)
	existing, err := readUserCrontab()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(existing))
	for _, l := range existing {
		if matchBSDCronLine(l, service) {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == len(existing) {
		return nil
	}
	return installUserCrontab(kept)
}

// readUserCrontab returns the invoking user's crontab lines. A user with no
// crontab yet yields no lines and no error.
func readUserCrontab() ([]string, error) {
	cmd := exec.Command("crontab", "-l")
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		// crontab exits non-zero with a specific message when none is
		// installed. That is an empty schedule, not a failure; anything else
		// (no crontab binary, a bad environment) is a real error so a
		// schedule change never silently overwrites an unreadable crontab.
		detail := strings.TrimSpace(errBuf.String())
		if isNoCrontabError(detail) {
			return nil, nil
		}
		return nil, fmt.Errorf("crontab -l: %w: %s", err, detail)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// isNoCrontabError reports whether crontab's failure means "you have no
// crontab installed" rather than a real error. The wording differs across
// releases and implementations (cronie vs vixie cron), so the check is on the
// distinctive phrase rather than an exact string, and any other failure is
// treated as real so a schedule change cannot clobber a crontab it could not
// read.
func isNoCrontabError(detail string) bool {
	return strings.Contains(strings.ToLower(detail), "no crontab")
}

// installUserCrontab writes lines to a temp file and hands it to crontab(1).
// Writing through `crontab -` would also work, but a file keeps the exact
// content inspectable if crontab rejects it.
func installUserCrontab(lines []string) error {
	f, err := os.CreateTemp("", "urnet-tools-crontab")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	for _, l := range lines {
		if _, err := f.WriteString(strings.TrimRight(l, "\n") + "\n"); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := exec.Command("crontab", f.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("crontab %s: %w (%s)", f.Name(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// toolExePath returns the running tool's absolute path for the crontab line.
// os.Executable is already indirected through the hotswap seam, so this reuses
// the same indirection the rest of the package has.
func toolExePath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return filepath.Clean(exe)
	}
	return "urnet-tools"
}

// renderBSDStatus runs `service <name> status`, the FreeBSD equivalent of
// `systemctl status <unit>`. It is installed into the shared seam so `status`
// prefers the real service view and falls back to the panel only when this
// returns an error.
func renderBSDStatus(p Provider) error {
	service := bsdRcServiceName(p.Binary)
	cmd := exec.Command("service", service, "status")
	cmd.Stdout = stdoutWriter()
	cmd.Stderr = stderrWriter()
	return cmd.Run()
}

func init() {
	renderSystemctlStatus = renderBSDStatus
}
