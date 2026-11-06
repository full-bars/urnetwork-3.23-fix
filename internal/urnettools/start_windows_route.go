package urnettools

import (
	"fmt"
	"path/filepath"
	"strings"
)

var (
	// startWindowsFn launches the provider (or runs its logon task) and
	// startSocketReachableFn probes its control socket; seams so tests
	// never start a real process or touch a real profile.
	startWindowsFn         = cmdStartWindows
	startSocketReachableFn = controlSocketReachable
)

// startProviderOnWindows is the start command's Windows route. Nothing
// owns the provider there, so start used to launch it (or run its logon
// task) unconditionally and a second start ran a second copy on the same
// profile. Like the FreeBSD rc script and systemd, an already-running
// provider is reported and start exits 0 without launching anything.
//
// A process-list failure does not block start: the control socket is
// still checked, and refusing every start on a transient snapshot error
// would leave the provider down.
func startProviderOnWindows(p Provider, force, dryRun bool) error {
	label := providerLabel(p)
	live, err := liveSharingProcesses()
	if err != nil {
		fmt.Printf("warning: could not list running provider processes (%v); checking the control socket only\n", err)
	}
	if len(live) > 0 {
		var running []string
		for _, pp := range live {
			running = append(running, pp.describe("start"))
		}
		fmt.Printf("%s is already running: %s; not starting a second copy\n", label, strings.Join(running, "; "))
		return nil
	}
	if startSocketReachableFn(p) {
		fmt.Printf("%s is already running (a provider answers on %s); not starting a second copy\n",
			label, filepath.Join(p.StateDir, "provider.sock"))
		return nil
	}
	return startWindowsFn(p, force, dryRun)
}
