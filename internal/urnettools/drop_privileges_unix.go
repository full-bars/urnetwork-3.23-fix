//go:build unix

package urnettools

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// dropPrivilegesTo runs cmd as username when the tool is root and the target
// differs from the caller (cross-user deployment: a root-run urnet-tools must
// not write the provider's jwt/network as root, or the provider cannot read it).
// No-op when already the target user, when the tool is not root, or when uid
// resolution fails (best-effort - the HOME-only pass-through below still runs).
func dropPrivilegesTo(username string, cmd *exec.Cmd) {
	if username == "" {
		return
	}
	target, err := user.Lookup(username)
	if err != nil {
		return
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(target.Gid)
	if err != nil {
		return
	}
	if isRootImpl() && uint32(os.Geteuid()) == uint32(uid) {
		return // already running as the target user
	}
	if !isRootImpl() {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
}

// isRootImpl reports whether the tool runs as uid 0.
func isRootImpl() bool {
	return os.Geteuid() == 0
}
