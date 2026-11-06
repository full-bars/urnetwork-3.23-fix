package urnettools

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// On Windows no unit owns the provider: it is a plain user process started
// by `start` or the logon task. Uninstall therefore has nothing like
// `systemctl disable --now` to stop it, and used to delete the binary and
// state dir while the provider still ran (the binary removal fails on the
// locked image, the state dir is half deleted under a live provider).
// stopProviderBeforeUninstall closes that gap: it stops the target, proves
// no provider process is left that could be using the same profile, and
// refuses to go on when it cannot prove that.

// providerProcess is one live provider process from a raw process listing.
// The listing is NOT deduplicated: discovery collapses every Windows
// provider process onto one row (they all report the default state dir),
// so it cannot show a second process that still holds the state dir.
type providerProcess struct {
	PID int
	Exe string
	// OwnerKnown is false when the process owner could not be read,
	// typically another account's process seen from a non-elevated prompt.
	OwnerKnown bool
	// OwnedByCaller is true when the process runs as the invoking user.
	OwnedByCaller bool
}

// mayShareProfile reports whether the process could be using the invoking
// user's provider state dir. Only a process positively identified as
// another account's is ruled out; an unreadable owner counts as a match.
func (pp providerProcess) mayShareProfile() bool {
	return !pp.OwnerKnown || pp.OwnedByCaller
}

func (pp providerProcess) describe() string {
	s := fmt.Sprintf("pid %d (%s)", pp.PID, pp.Exe)
	if !pp.OwnerKnown {
		s += ", owner unknown: if it belongs to another Windows account, run uninstall from an elevated prompt so the tool can tell"
	}
	return s
}

var (
	// uninstallStopsProcesses selects the process-stop step in cmdUninstall.
	// Windows only: systemd/rc.d/launchd own the provider everywhere else.
	uninstallStopsProcesses = runtime.GOOS == "windows"
	// listProviderProcessesFn lists live provider processes without dedup.
	listProviderProcessesFn = listProviderProcesses
	// uninstallStopFn stops the target. Graceful first; the Windows stop
	// path escalates to TerminateProcess after its own timeout.
	uninstallStopFn = func(p Provider) error { return cmdStopWindows(p, false, false) }
	// uninstallSocketReachableFn reports whether a provider still answers
	// on the target's control socket.
	uninstallSocketReachableFn = controlSocketReachable
	// uninstallExitWait bounds how long uninstall waits for the stopped
	// provider to disappear from the process list.
	uninstallExitWait     = 10 * time.Second
	uninstallPollInterval = 250 * time.Millisecond
	uninstallSleepFn      = time.Sleep
	// cleanupLifecycleFn removes the platform lifecycle artifacts (Windows
	// scheduled tasks, Unix timers). A seam so tests never touch real ones.
	cleanupLifecycleFn = cleanupLifecycle
)

// liveSharingProcesses lists the live provider processes that could be
// using the invoking user's profile.
func liveSharingProcesses() ([]providerProcess, error) {
	all, err := listProviderProcessesFn()
	if err != nil {
		return nil, err
	}
	var out []providerProcess
	for _, pp := range all {
		if pp.mayShareProfile() {
			out = append(out, pp)
		}
	}
	return out, nil
}

// stopProviderBeforeUninstall stops the target provider and proves that no
// provider process which could share its profile is still alive. It
// returns an error, before anything is removed, whenever that cannot be
// proven. It never stops a provider other than the target.
func stopProviderBeforeUninstall(p Provider) error {
	label := providerLabel(p)
	nothingRemoved := "nothing was stopped or removed"

	live, err := liveSharingProcesses()
	if err != nil {
		return fmt.Errorf("uninstall aborted: could not list running provider processes: %v\n%s; run uninstall again, or stop the provider first: %s stop",
			err, nothingRemoved, programName())
	}

	// Another provider process may be using the same state dir. Stopping
	// the target and deleting the files would still break it, so refuse
	// before touching anything.
	var others []string
	targetLive := false
	for _, pp := range live {
		if p.PID > 0 && pp.PID == p.PID {
			targetLive = true
			continue
		}
		others = append(others, pp.describe())
	}
	if len(others) > 0 {
		return fmt.Errorf("uninstall aborted: another provider process is running and may use the same state dir (%s):\n  %s\n%s; stop it first (%s stop, or end it in Task Manager), then run uninstall again",
			p.StateDir, strings.Join(others, "\n  "), nothingRemoved, programName())
	}

	if !targetLive && !uninstallSocketReachableFn(p) {
		fmt.Printf("%s is not running; nothing to stop\n", label)
		return nil
	}

	fmt.Printf("stopping %s before removing its files...\n", label)
	if err := uninstallStopFn(p); err != nil {
		return fmt.Errorf("uninstall aborted: could not stop %s: %v\nnothing was removed; stop it (%s stop, or end it in Task Manager), then run uninstall again",
			label, err, programName())
	}

	// Prove it is gone: no sharing process left and nothing answering on
	// the control socket. TerminateProcess returns before the process
	// object is fully gone, so poll for a bounded time.
	attempts := int(uninstallExitWait / uninstallPollInterval)
	var socketUp bool
	for i := 0; ; i++ {
		live, err = liveSharingProcesses()
		if err != nil {
			return fmt.Errorf("uninstall aborted: stopped %s but could not confirm it exited: %v\nnothing was removed; check Task Manager for urnetwork.exe, then run uninstall again",
				label, err)
		}
		socketUp = uninstallSocketReachableFn(p)
		if len(live) == 0 && !socketUp {
			fmt.Printf("stopped %s; no provider process is running\n", label)
			return nil
		}
		if i >= attempts {
			break
		}
		uninstallSleepFn(uninstallPollInterval)
	}

	var still []string
	for _, pp := range live {
		still = append(still, pp.describe())
	}
	if socketUp {
		still = append(still, "a provider still answers on "+filepath.Join(p.StateDir, "provider.sock"))
	}
	return fmt.Errorf("uninstall aborted: %s is still running after the stop:\n  %s\nnothing was removed; end it in Task Manager, then run uninstall again",
		label, strings.Join(still, "\n  "))
}
