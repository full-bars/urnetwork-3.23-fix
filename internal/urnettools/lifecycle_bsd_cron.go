package urnettools

import (
	"fmt"
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
