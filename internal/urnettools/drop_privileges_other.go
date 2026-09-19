//go:build !unix

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
)

// dropPrivilegesTo cannot drop privileges on platforms without POSIX setuid,
// so there is nothing to configure on the child. It is a no-op for the two
// cases that need no dropping (empty user, or a caller that is not root) and
// reports an error only when a root caller asks to run as a different user —
// mirroring dropPrivilegesTo's unix contract: failure is an error, never a
// silent root fallback. On Windows os.Geteuid() is always -1, so the no-op
// path is the normal one.
func dropPrivilegesTo(username string, cmd *exec.Cmd) error {
	if username == "" || os.Geteuid() != 0 {
		return nil
	}
	return fmt.Errorf("dropPrivilegesTo is unsupported on this platform (user %q)", username)
}
