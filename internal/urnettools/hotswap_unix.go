//go:build !windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// triggerHotSwap signals the running provider process on Unix via SIGUSR2 to initiate
// an in-process verified handoff.
func triggerHotSwap(p Provider) error {
	if p.PID <= 0 {
		return errors.New("provider has no valid PID")
	}
	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return fmt.Errorf("find process %d: %w", p.PID, err)
	}
	if err := proc.Signal(syscall.SIGUSR2); err != nil {
		return fmt.Errorf("send SIGUSR2 to PID %d: %w", p.PID, err)
	}
	return nil
}
