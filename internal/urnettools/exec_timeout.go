package urnettools

import (
	"context"
	"os/exec"
	"time"
)

// execWithTimeout runs a command with a deadline, preventing wedged
// systemctl/getent/docker subprocesses from hanging the entire tool.
func execWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}
