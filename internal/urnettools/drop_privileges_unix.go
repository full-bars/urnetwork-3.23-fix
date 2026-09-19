//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// dropPrivilegesTo runs cmd as username when the tool is root and the target
// differs from the caller (cross-user deployment: a root-run urnet-tools must
// not write the provider's jwt/network as root, or the provider cannot read it).
// No-op when already the target user or when the tool is not root.
//
// FAILURE IS AN ERROR, never a silent root fallback: a root-run tool that
// cannot resolve the target user must not go on to execute the provider
// binary as root — C1 (a discovery User whose lookup fails, e.g. an
// attacker-supplied USER= value, would otherwise run an arbitrary binary
// with full privileges and HOME=/root). The caller decides whether to
// proceed unprivileged (e.g. only as the operator's own uid).
func dropPrivilegesTo(username string, cmd *exec.Cmd) error {
	if username == "" {
		return nil
	}
	if !isRootImpl() {
		return nil
	}
	target, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("resolve user %q: %w", username, err)
	}
	// 32-bit bound (parseUnixID): Atoi accepts values above uint32 on 64-bit
	// platforms, and uint32(4294967296) wraps to 0, which would make the
	// "already the target user" check below pass for root and run the
	// command as root.
	uid, err := parseUnixID(target.Uid)
	if err != nil {
		return fmt.Errorf("parse uid for %q: %w", username, err)
	}
	gid, err := parseUnixID(target.Gid)
	if err != nil {
		return fmt.Errorf("parse gid for %q: %w", username, err)
	}
	if uint32(os.Geteuid()) == uid {
		return nil // already running as the target user
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	return nil
}

// parseUnixID parses a decimal uid/gid, rejecting anything that does not fit
// in uint32 (the width of syscall.Credential's fields).
func parseUnixID(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// isRootImpl reports whether the tool runs as uid 0.
func isRootImpl() bool {
	return os.Geteuid() == 0
}
