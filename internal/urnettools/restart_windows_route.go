package urnettools

import "fmt"

var (
	// restartWindowsAliveFn and restartProviderWindowsFn are seams so tests
	// never stop or launch a real process.
	restartWindowsAliveFn    = pidIsAlive
	restartProviderWindowsFn = restartProviderWindows
)

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
	return restartProviderWindowsFn(p)
}
