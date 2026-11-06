package urnettools

import (
	"fmt"
	"strings"
)

var (
	// restartWindowsAliveFn and restartProviderWindowsFn are seams so tests
	// never stop or launch a real process.
	restartWindowsAliveFn    = pidIsAlive
	restartProviderWindowsFn = restartProviderWindows
	// restartWindowsCmdFn is the restart command's stop/start, a seam for
	// the same reason.
	restartWindowsCmdFn = cmdRestartWindows
)

// refuseRestartWhileProfileShared returns an error, before anything is
// stopped, when a provider process other than the target may share its
// state dir. The stop half of a restart talks to provider.sock, and with
// two processes on one profile the socket may belong to the other one: the
// restart would shut that provider down and start a duplicate of the
// target. Like uninstall, it fails closed when the process list cannot be
// read.
func refuseRestartWhileProfileShared(p Provider) error {
	return refuseControlWhileProfileShared(p, "restart")
}

func refuseControlWhileProfileShared(p Provider, verb string) error {
	label := providerLabel(p)
	live, err := liveSharingProcesses()
	if err != nil {
		return fmt.Errorf("could not %s provider %s: could not list running provider processes to check that no other provider shares its state dir: %v\nnothing was stopped; check the process list and try again",
			verb, label, err)
	}
	var others []string
	for _, pp := range live {
		if p.PID > 0 && pp.PID == p.PID {
			continue
		}
		others = append(others, pp.describe(verb))
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("could not %s provider %s: another provider process is running and may use the same state dir (%s):\n  %s\nthe control socket may belong to that process, so this could stop the wrong provider; nothing was stopped. End the extra process in Task Manager, then run: %s %s",
		verb, label, p.StateDir, strings.Join(others, "\n  "), programName(), verb)
}

// cmdRestartOnWindows is the restart command's Windows route.
func cmdRestartOnWindows(p Provider, force, dryRun bool) error {
	if err := refuseRestartWhileProfileShared(p); err != nil {
		return err
	}
	return restartWindowsCmdFn(p, force, dryRun)
}

// restartProviderOnWindows is restartProvider's Windows route (update and
// tune restarts). No unit owns the provider there, so the systemd path
// always failed with "no systemd unit resolved" and left the old image
// running. A live provider is stopped and started again; a stopped one is
// left stopped, so an unattended update never relaunches a provider the
// operator stopped on purpose.
func restartProviderOnWindows(p Provider) error {
	if p.PID <= 0 || !restartWindowsAliveFn(p.PID) {
		return fmt.Errorf("could not restart provider %s: it is not running; the change takes effect when you start it: %s start",
			providerLabel(p), programName())
	}
	if err := refuseRestartWhileProfileShared(p); err != nil {
		return err
	}
	return restartProviderWindowsFn(p)
}
