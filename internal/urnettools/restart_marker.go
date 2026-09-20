package urnettools

import (
	"fmt"
	"os"
	"time"
)

// restartReasonFile is the marker `urnet-tools` leaves in a provider's state
// dir just before it restarts that provider. The provider reads and deletes
// it at its next start (ignoring one older than 10 minutes) to report WHY it
// restarted. One line: "<reason> <RFC3339 UTC time>".
const restartReasonFile = ".restart-reason"

// Reasons this tool records. Others (clean, unclean, first-start) are decided
// by the provider itself.
const (
	restartReasonUpdate  = "update"
	restartReasonHotswap = "hotswap"
	restartReasonManual  = "manual"
)

// restartMarkerLine renders the marker content for reason at now.
func restartMarkerLine(reason string, now time.Time) []byte {
	return []byte(fmt.Sprintf("%s %s\n", reason, now.UTC().Format(time.RFC3339)))
}

// writeRestartMarkerIn writes the marker into stateDir. This runs as root
// against a directory the provider user owns and can rename or re-link at any
// time, so the write goes through a descriptor-pinned handle (never a
// pathname write): a symlinked state dir or component, or a symlink planted at
// the marker name, is refused instead of followed, and the file is handed to
// the directory's owner so the unprivileged provider can delete it. root is
// the trusted home the state dir lies beneath (Provider.StateHome), or "".
func writeRestartMarkerIn(root, stateDir, reason string, now time.Time) error {
	switch reason {
	case restartReasonUpdate, restartReasonHotswap, restartReasonManual:
	default:
		return fmt.Errorf("unknown restart reason %q", reason)
	}
	if stateDir == "" {
		return fmt.Errorf("no state dir")
	}
	h, err := openStateDirIn(root, stateDir)
	if err != nil {
		return err
	}
	defer h.Close()
	return h.writeOwned(restartReasonFile, restartMarkerLine(reason, now), 0o644)
}

// recordRestartReason leaves the marker for p before a restart. It is best
// effort by design: a failed write costs only the restart reason in the live
// status, so it prints a note and never aborts the restart or update.
func recordRestartReason(p Provider, reason string) {
	if p.StateDir == "" {
		return
	}
	if err := writeRestartMarkerIn(p.StateHome, p.StateDir, reason, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not record restart reason (%s): %v\n", reason, err)
	}
}
