//go:build linux

package urnettools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
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

	tmp := fmt.Sprintf("%s.recover-%d", binary, pid)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("copy binary from %s: %v", procExe, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %v", tmp, closeErr)
	}
	if n == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("recovered binary is empty (source %s gave 0 bytes)", procExe)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, binary); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, binary, err)
	}

	// Best-effort ownership fix when we are root and the provider runs as
	// another user: the recreated file would otherwise be root-owned and break
	// the provider's own read/exec of its binary. Not fatal on failure.
	if os.Geteuid() == 0 && user != "" {
		if uid, gid, err := lookupUserIDs(user); err == nil {
			_ = os.Chown(binary, uid, gid)
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
