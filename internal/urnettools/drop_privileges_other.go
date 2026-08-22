//go:build !unix

package urnettools

import "os/exec"

// dropPrivilegesTo is a no-op on platforms without POSIX setuid.
func dropPrivilegesTo(username string, cmd *exec.Cmd) {}
