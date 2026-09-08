//go:build !windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrHotSwapNotSupported is returned when the running provider process does not support zero-downtime hotswap.
var ErrHotSwapNotSupported = errors.New("running provider does not support zero-downtime hotswap (requires >= v3.23.0-fix.31.0)")

// ErrHotSwapUnitNotNotify is returned when the provider's version supports
// HotSwap but its owning systemd unit is not Type=notify, so
// provider/hotswap.go would abort the in-process handoff internally rather
// than actually hand off (see hotSwapUnitOK for the mechanism). Naming a
// version here would be actively misleading — the fix is reinstalling the
// systemd unit, not upgrading the binary.
var ErrHotSwapUnitNotNotify = errors.New("provider's systemd unit is not Type=notify, so zero-downtime hotswap cannot complete — reinstall/refresh the systemd unit (re-run the installer, or urnet-tools install) to pick up Type=notify")

// triggerHotSwap signals the running provider process on Unix via SIGUSR2 to initiate
// an in-process verified handoff.
func triggerHotSwap(p Provider) error {
	if p.PID <= 0 {
		return errors.New("provider has no valid PID")
	}
	if !hotSwapVersionOK(p) {
		return ErrHotSwapNotSupported
	}
	if !hotSwapUnitOK(p) {
		return ErrHotSwapUnitNotNotify
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

// pidIsAlive reports whether pid still refers to a running process. Used by
// the update-rollback path to distinguish "HotSwap handoff never happened"
// (oldPID still alive, safe to roll back the on-disk binary) from "handoff
// happened but our verification loop just didn't observe it in time"
// (oldPID already exited its drain and is gone — rolling back now would
// only affect a future restart, but the message that "the live provider was
// never killed" would be false).
func pidIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, os.FindProcess always succeeds; signal 0 does no actual
	// signaling, just existence/permission checks.
	return proc.Signal(syscall.Signal(0)) == nil
}
