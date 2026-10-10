//go:build linux

package main

import (
	"context"
	"time"
)

// livenessOpenRcTick is how often the stall check runs under OpenRC. The budget
// it compares against is livenessStaleAfter (ten minutes), so a minute is plenty
// and each tick costs one atomic load. A var so tests can drive it.
var livenessOpenRcTick = time.Minute

// livenessUnderOpenRc reports whether the provider sits under OpenRC's
// supervise-daemon, which restarts on exit 75 but has no watchdog of its own.
func livenessUnderOpenRc() bool {
	kind, restartsOn75 := thrashSupervisorKind(thrashEnvFn, thrashFileExistsFn)
	return kind == "openrc" && restartsOn75
}

// runOpenRcLivenessWatchdog is the OpenRC half of the stall defense. There is no
// watchdog to starve: supervise-daemon has no sd_notify, so a provider that is
// alive but making no progress is never restarted unless it asks for it itself.
// The action is therefore the same self-exit the thrash watchdog uses (status
// 75, which supervise-daemon turns into a restart), guarded by the same
// self-heal switch and the same daily restart budget, and it leaves the same
// lean start cap behind.
func runOpenRcLivenessWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
	ticker := time.NewTicker(livenessOpenRcTick)
	defer ticker.Stop()
	passive, acting := false, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if processLiveness.fresh(livenessStaleAfter) {
			if passive || acting {
				passive, acting = false, false
				critLog("[liveness] progress resumed; the OpenRC restart was not taken\n")
			}
			continue
		}
		ok, why := gate(!acting)
		if !ok {
			if !passive {
				passive = true
				critLog("[liveness] no progress for %s, but %s, so the provider keeps running (under OpenRC it would exit %d and be restarted)\n", livenessStaleAfter, why, thrashExitCode)
			}
			continue
		}
		if !acting {
			acting = true
			critLog("🚨 [liveness] no progress for %s: exiting %d so supervise-daemon restarts the provider\n", livenessStaleAfter, thrashExitCode)
			_ = recordLivenessStall()
			thrashExitFn(thrashExitCode)
		}
	}
}
