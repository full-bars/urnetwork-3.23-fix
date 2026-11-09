//go:build !windows

package urnettools

import "syscall"

// reExecTool replaces the current process image with the newly installed tool
// binary via execve. On success it never returns; on failure it returns the
// execve errno so the caller can report it and fall back to the running binary.
func reExecTool(exe string, args []string, env []string) error {
	return syscall.Exec(exe, args, env)
}
