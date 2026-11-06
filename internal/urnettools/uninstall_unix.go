package urnettools

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// On Unix the provider is not always owned by a systemd unit: it may be a
// manual or nohup launch, an OpenRC service or a FreeBSD rc.d service.
// Uninstall used to call `systemctl disable --now` only when a unit was
// known and merely warned when that failed, so every other launch style kept
// running while its binary and state dir were deleted.
// stopProviderUnixBeforeUninstall stops the target through whatever owns it,
// proves it exited, and refuses to go on (before anything is removed) when it
// cannot.

// uninstallPidAliveFn reports whether a pid may still be running. It fails
// closed: a process that cannot be signalled (EPERM) still counts as alive.
var uninstallPidAliveFn = processAliveStrict

// systemdManaged reports whether the target is owned by a systemd unit
// rather than by the OpenRC service that shares the Unit field.
func systemdManaged(p Provider) bool {
	if p.Unit == "" {
		return false
	}
	if openrcActive() && (p.Unit == openrcServiceName || p.Unit == openrcServiceName+".service") {
		return false
	}
	return true
}

// defaultUninstallStop stops a provider that no systemd unit owns, through
// its real owner: the rc.d service on FreeBSD, the OpenRC service, else a
// graceful signal to the bare process.
func defaultUninstallStop(p Provider) error {
	if runtime.GOOS == "windows" {
		return cmdStopWindows(p, false, false)
	}
	if runtime.GOOS == "freebsd" {
		if err := bsdServiceControl(p, "stop"); err == nil {
			return nil
		} else if p.PID <= 0 {
			return err
		}
		// No usable rc.d script: it is a bare process, signal it.
	}
	if p.Unit == openrcServiceName || p.Unit == openrcServiceName+".service" {
		if openrcActive() && openrcServiceInstalled() {
			return runOpenRCCommand(openrcServiceArgv("stop")...)
		}
	}
	return stopUnitlessProvider(p, false)
}

func stopProviderUnixBeforeUninstall(p Provider) error {
	label := providerLabel(p)
	if systemdManaged(p) {
		args := []string{"disable", "--now", p.Unit}
		if isUserUnit(p.Unit) && p.User != "" {
			args = append(systemctlUserArgs(p.User), args...)
		}
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("uninstall aborted: could not disable %s: %v (%s)\nnothing was removed; fix the unit (%s stop), then run uninstall again",
				p.Unit, err, strings.TrimSpace(string(out)), programName())
		}
	} else if p.Running || p.PID > 0 || uninstallSocketReachableFn(p) {
		fmt.Printf("stopping %s before removing its files...\n", label)
		if err := uninstallStopFn(p); err != nil {
			return fmt.Errorf("uninstall aborted: could not stop %s: %v\nnothing was removed; stop it (%s stop), then run uninstall again",
				label, err, programName())
		}
	}

	// Prove it is gone. A pid that cannot be probed counts as alive and the
	// control socket must be silent; a process list is never used as proof
	// because it silently omits processes the caller cannot inspect.
	attempts := int(uninstallExitWait / uninstallPollInterval)
	for i := 0; ; i++ {
		pidUp := p.PID > 0 && uninstallPidAliveFn(p.PID)
		socketUp := uninstallSocketReachableFn(p)
		if !pidUp && !socketUp {
			return nil
		}
		if i >= attempts {
			var still []string
			if pidUp {
				still = append(still, fmt.Sprintf("pid %d is still running or cannot be inspected", p.PID))
			}
			if socketUp {
				still = append(still, "a provider still answers on its control socket")
			}
			return fmt.Errorf("uninstall aborted: %s is still running after the stop (%s)\nnothing was removed; stop it (%s stop --force), then run uninstall again",
				label, strings.Join(still, "; "), programName())
		}
		uninstallSleepFn(uninstallPollInterval)
	}
}
