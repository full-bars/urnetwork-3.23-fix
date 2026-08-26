package urnettools

import (
	"fmt"
	"os"
)

// ensureBinaryRecoverable guarantees the provider binary exists on disk before
// a delegation exec. Normally it already does. When an update renamed the
// running executable aside and a later step deleted that file, the provider
// process keeps running on the deleted inode — the disk path is gone but
// /proc/<pid>/exe still resolves the live image. In that state a plain exec
// fails with "fork/exec ... no such file or directory", breaking every
// pass-through command (proxy add/remove/refresh, summary, auth, ...).
//
// This restores the binary from the running process so commands keep working
// against the ACTIVE provider — the operator's explicit requirement: an
// updated-but-not-restarted provider must stay operable, with no restart.
//
// Returns (binary, recovered, err). When the binary exists it is returned
// untouched (recovered=false) — the 99% case, zero behavior change. Recovery
// itself is platform-specific (Linux /proc) and lives behind holdRecoverDeletedBinary.
func ensureBinaryRecoverable(p Provider) (string, bool, error) {
	if _, err := os.Stat(p.Binary); err == nil {
		return p.Binary, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("provider %s: stat %s: %w", providerLabel(p), p.Binary, err)
	}

	// Binary missing on disk. Recoverable only while the process still runs.
	if p.PID <= 0 || !p.Running {
		return "", false, fmt.Errorf(
			"provider %s: binary %s is missing on disk and no running process remains to recover it from — restart the unit or reinstall",
			providerLabel(p), p.Binary)
	}

	if err := recoverDeletedBinary(p.Binary, p.PID, p.User); err != nil {
		return "", false, fmt.Errorf(
			"provider %s: binary %s is missing on disk (a prior update deleted it) but its process still runs; auto-recovery failed: %v — rerun as root to allow recovery, or restart the unit",
			providerLabel(p), p.Binary, err)
	}
	return p.Binary, true, nil
}
