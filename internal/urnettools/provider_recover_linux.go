//go:build linux

package urnettools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// recoverDeletedBinary restores a deleted-but-running provider binary from the
// process's /proc/<pid>/exe symlink. The kernel keeps the image inode open for
// the lifetime of the process even after the on-disk file was removed, so the
// symlink still yields the exact bytes the process is executing.
//
// Hard guard: the symlink target (minus the kernel's " (deleted)" marker) must
// equal the requested binary path exactly. Otherwise the process no longer
// corresponds to `binary` (PID reused, path drifted) and we refuse to recreate
// a file we might not own, rather than clobber a path that belongs to a
// different binary.
//
// The write is atomic (copy to a temp sibling, then rename) so an in-flight
// update installing a fresh binary at the same path can never be truncated by
// a half-written recovery. When we are root the recreated file is chowned to
// the provider user, so the provider keeps reading/exec'ing its own binary.
func recoverDeletedBinary(binary string, pid int, user string) error {
	procExe := "/proc/" + strconv.Itoa(pid) + "/exe"
	target, err := os.Readlink(procExe)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", procExe, err)
	}
	if resolved := strings.TrimSuffix(target, " (deleted)"); resolved != binary {
		return fmt.Errorf("/proc/%d/exe resolves to %q, expected %q — refusing to recover", pid, resolved, binary)
	}

	in, err := os.Open(procExe)
	if err != nil {
		return fmt.Errorf("open %s: %w", procExe, err)
	}
	defer in.Close()

	// If the provider process exits mid-copy, /proc/<pid>/exe yields a
	// TRUNCATED image. fstat the open handle for its real size and require a
	// full read; a short, non-zero copy would otherwise pass the n==0 check and
	// install a corrupt executable (nemotron review #4).
	want := int64(-1)
	if fi, serr := in.Stat(); serr == nil {
		want = fi.Size()
	}

	// Unique temp sibling in the same directory (atomic rename must stay on
	// one filesystem). CreateTemp keeps the name collision-free even if two
	// providerSubcommand calls race for the same provider (mimo review LOW);
	// OpenFile(O_TRUNC) on a PID-derived name could truncate a peer's in-flight
	// copy. Mode 0600 until the final Chmod(0o755) below.
	tmp := ""
	if out, err := os.CreateTemp(filepath.Dir(binary), filepath.Base(binary)+".recover-*"); err != nil {
		return fmt.Errorf("create temp for %s: %w", binary, err)
	} else {
		tmp = out.Name()
		if n, copyErr := io.Copy(out, in); copyErr != nil {
			out.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("copy binary from %s: %v", procExe, copyErr)
		} else if n == 0 {
			out.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("recovered binary is empty (source %s gave 0 bytes)", procExe)
		} else if want > 0 && int64(n) != want {
			out.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("recovered binary truncated during copy (got %d bytes, expected %d from %s)", n, want, procExe)
		}
		if closeErr := out.Close(); closeErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("close %s: %v", tmp, closeErr)
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	// Belt-and-braces: confirm the recovered bytes are a valid executable for
	// this platform before installing (catches a truncated/corrupt image the
	// size check or the process-exit race could slip through).
	if !isRecognizedExecutable(tmp) {
		_ = os.Remove(tmp)
		return fmt.Errorf("recovered binary is not a valid %s executable — refusing to install", runtime.GOOS)
	}

	if err := installRecovered(tmp, binary); err != nil {
		return fmt.Errorf("install recovered binary: %w", err)
	}

	// Best-effort ownership fix when we are root and the provider runs as
	// another user: the recreated file would otherwise be root-owned and break
	// the provider's own read/exec of its binary. Not fatal on failure.
	if os.Geteuid() == 0 && user != "" {
		if uid, gid, err := lookupUserIDs(user); err == nil {
			if cerr := os.Chown(binary, uid, gid); cerr != nil {
				// Not fatal, but surface it: a root-owned recovered binary can
				// make the provider's own read/exec fail (mimo review NIT).
				fmt.Fprintf(os.Stderr, "note: recovered binary %s is root-owned (chown to %s failed: %v) — provider may fail to exec it\n", binary, user, cerr)
			}
		}
	}
	return nil
}

// lookupUserIDs resolves a user's numeric uid/gid via `id`, mirroring the
// existing pattern used by the update install path.
func lookupUserIDs(user string) (int, int, error) {
	uidOut, err := exec.Command("id", "-u", user).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("id -u %s: %w", user, err)
	}
	gidOut, err := exec.Command("id", "-g", user).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("id -g %s: %w", user, err)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(uidOut)))
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid for %s: %w", user, err)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(string(gidOut)))
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid for %s: %w", user, err)
	}
	return uid, gid, nil
}
