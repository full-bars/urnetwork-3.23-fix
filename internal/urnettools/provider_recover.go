package urnettools

import (
	"fmt"
	"os"
	"runtime"
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
		// %w (not %v) so a caller can errors.Is on errBinaryAppeared and tell
		// "a concurrent updater won the race" apart from a genuine recovery
		// failure. The root hint is Linux-only: root can
		// read another account's /proc there, but it cannot enable recovery on
		// macOS/Windows.
		hint := "restart the unit, or reinstall"
		if runtime.GOOS == "linux" {
			hint = "rerun as root to allow recovery, or restart the unit"
		}
		return "", false, fmt.Errorf(
			"provider %s: binary %s is missing on disk (a prior update deleted it) but its process still runs; auto-recovery failed: %w — %s",
			providerLabel(p), p.Binary, err, hint)
	}
	return p.Binary, true, nil
}

// errBinaryAppeared is returned by installRecovered when the target path
// materialized between recovery's initial check and its final install — a
// concurrent updater won the race, so the recovered (stale) image must not
// overwrite the fresh binary that legitimate update just placed there.
var errBinaryAppeared = fmt.Errorf("provider binary reappeared during recovery; refusing to overwrite")

// installRecovered atomically moves tmp into binary, refusing to clobber a file
// that appears between recovery construction and install. It lives in the
// shared (non-build-tagged) file so the package's tests and the Windows
// `go test` step compile it on every platform (os.Lstat/os.Rename are
// cross-platform; the RENAME_NOREPLACE atomic fast path is Linux-only and is
// not used here). This narrows the stale-vs-fresh TOCTOU to a single
// lstat+rename — an accepted residual. If an updater wins the race, recovery yields rather than overwrite a
// freshly-installed binary with the deleted-inode image.
func installRecovered(tmp, binary string) error {
	if _, err := os.Lstat(binary); err == nil {
		_ = os.Remove(tmp)
		return errBinaryAppeared
	} else if !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return fmt.Errorf("stat %s: %w", binary, err)
	}
	if err := os.Rename(tmp, binary); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, binary, err)
	}
	return nil
}
