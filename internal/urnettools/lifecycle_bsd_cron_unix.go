//go:build freebsd || openbsd

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Cron-driven auto-update shared by the FreeBSD and OpenBSD lifecycles.
//
// Neither system has systemd timers, so a schedule is a crontab line. That is
// weaker in one way worth stating: cron has no missed-run catch-up, so a box
// that was down at the scheduled minute does not update on the next boot the
// way a systemd timer would have.

// setAutoUpdateSchedule installs or removes the cron entry for auto-update.
//
// label is the platform-neutral identifier the caller computed. FreeBSD derives
// its own name from the service instead, matching how the Linux timer name is
// derived from the provider unit rather than from the label.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	euid := os.Geteuid()
	if euid != 0 && bsdTreeOwnedByRoot(p.Binary) {
		return fmt.Errorf("auto-update for %s must be scheduled as root: %s is in the root-owned install tree, so an unprivileged cron job cannot replace it (run: sudo urnet-tools auto-update %s)", providerLabel(p), p.Binary, interval)
	}
	if interval == "off" {
		return removeBSDUpdateCron(p)
	}
	tool, logDir, err := bsdScheduledUpdate(euid, bsdStagedToolIsRootOwned(), toolExePath(), bsdLogDir())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("auto-update: create log dir %s: %w", logDir, err)
	}
	selector, err := bsdUpdateSelector(p)
	if err != nil {
		return err
	}
	line, err := bsdUpdateCronLine(tool, logDir, interval, selector)
	if err != nil {
		return err
	}
	return writeBSDUpdateCron(p, line)
}

// bsdStagedToolIsRootOwned reports whether the installer's staged tool copy
// exists as a regular file owned by root. Existence alone is not enough: a file
// planted at that path by another user must not qualify it for root's crontab.
func bsdStagedToolIsRootOwned() bool {
	fi, err := os.Lstat(bsdRootToolPath)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && fi.Mode().Perm()&0o022 == 0 && fi.Mode().Perm()&0o111 != 0
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
	lines = strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
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
