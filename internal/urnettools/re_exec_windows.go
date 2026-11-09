//go:build windows

package urnettools

import (
	"os"
	"os/exec"
)

// reExecTool on Windows cannot replace the process image in-place (no execve).
// It spawns the replacement executable as a child process with standard streams
// connected and wait-exits forwarding the child's exit status.
func reExecTool(exe string, args []string, env []string) error {
	cmd := exec.Command(exe, args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
