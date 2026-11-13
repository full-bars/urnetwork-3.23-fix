package urnettools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The FreeBSD auto-update crontab bookkeeping, kept untagged so the matching
// RULE is unit-testable on any platform. The functions that use it
// (writeBSDUpdateCron / removeBSDUpdateCron) only exist on FreeBSD.

// bsdCronMarker tags every line this tool writes so a schedule change can
// remove its own lines without touching anything else in the crontab.
const bsdCronMarker = "# urnet-tools auto-update"

// matchBSDCronLine reports whether a crontab line is the auto-update entry for
// this specific service.
//
// The marker must be matched at the END of the line. bsdCronMarker is a common
// prefix of every marker written, so a Contains match on "... auto-update
// urnetwork" also matches "... auto-update urnetwork_beta" — which would let a
// schedule change for the stable provider delete the beta provider's entry on a
// box running both. Trimming first makes a trailing space harmless.
func matchBSDCronLine(line, service string) bool {
	return strings.HasSuffix(strings.TrimSpace(line), bsdCronMarker+" "+service)
}

// bsdInstallRoot is the root-owned tree the FreeBSD installer creates. It must
// match install_path in scripts/Provider_Install_FreeBSD.sh.
const bsdInstallRoot = "/usr/local/libexec/urnetwork-provider"

// bsdTreeOwnedByRoot reports whether a binary path lives under the root-owned
// FreeBSD layout. installBinary consults this before chowning a freshly swapped
// binary to the service user: chowning a file in the root-owned tree would hand
// the file itself to an unprivileged account that root later executes (the
// update-time version verify, the next sudo update). The service user only
// needs read and exec, which the 0755 already gives it.
func bsdTreeOwnedByRoot(path string) bool {
	return path == bsdInstallRoot || strings.HasPrefix(path, bsdInstallRoot+"/")
}

// bsdRootToolPath is the root-owned urnet-tools copy the FreeBSD installer
// stages inside the root-owned tree.
const bsdRootToolPath = bsdInstallRoot + "/bin/urnet-tools"

// bsdRootLogDir is where root's unattended update log lives. Root must not log
// into a service-user home: that directory is user-writable, so a symlink at
// the log name would redirect root's append.
const bsdRootLogDir = "/var/log/urnetwork"

// bsdScheduledUpdate picks the tool path and log dir for a cron entry.
// stagedToolOK reports whether the installer's root-owned tool copy exists and
// is root-owned. A non-root crontab runs as the user who wrote it, so their own
// tool path is not an escalation. Root's crontab must only ever execute a path
// no other user can influence, so root uses the staged copy or gets an error,
// with no silent fallback to the invoker's binary.
func bsdScheduledUpdate(euid int, stagedToolOK bool, runningTool, userLogDir string) (tool, logDir string, err error) {
	if euid != 0 {
		return runningTool, userLogDir, nil
	}
	if !stagedToolOK {
		return "", "", fmt.Errorf("refusing to install root's auto-update: no root-owned urnet-tools at %s, so install it there first (the installer stages it) or run auto-update as your own user", bsdRootToolPath)
	}
	return bsdRootToolPath, bsdRootLogDir, nil
}

// bsdUpdateCronLine returns the crontab line for an auto-update interval.
//
// The command runs `urnet-tools update -f <selector>`, the same thing the
// systemd timer runs plus an explicit target, and appends to a log so an
// unattended update that fails leaves a trail
// instead of vanishing. An unknown interval is rejected rather than defaulted:
// silently installing a daily schedule for someone who asked for weekly is worse
// than an error.
func bsdUpdateCronLine(tool, logDir, interval string, selector []string) (string, error) {
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
	args := "-f"
	for _, a := range selector {
		args += " " + shellQuote(a)
	}
	command := fmt.Sprintf("%s update %s >> %s 2>&1",
		shellQuote(tool), args, shellQuote(filepath.Join(logDir, "auto-update.log")))
	// Empty shell quotes separate a preceding backslash from cron's escape.
	return spec + " " + strings.ReplaceAll(command, "%", `''\%`), nil
}

// bsdUpdateSelector names the provider the scheduled update must act on. Without
// it `update -f` resolves its target at run time (a persisted default, or the
// caller's only running provider), so a marker on the crontab line would not
// pin the entry to its provider. The state dir is unique per provider, so it is
// preferred over the user name.
func bsdUpdateSelector(p Provider) ([]string, error) {
	switch {
	case p.StateDir != "":
		return []string{"--state-dir", p.StateDir}, nil
	case p.User != "":
		return []string{"--user", p.User}, nil
	}
	return nil, fmt.Errorf("auto-update for %s: the provider has no state dir or user to target, so the schedule would not name its provider", providerLabel(p))
}

// shellQuote wraps s in single quotes for inclusion in a crontab line, which is
// executed by /bin/sh. A path containing a space or a quote must not break the
// schedule.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
