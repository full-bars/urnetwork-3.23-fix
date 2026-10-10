//go:build linux

package main

import (
	"context"
	"time"
)

// livenessSelfExitTick is how often the stall check runs where the supervisor
// has no watchdog to withhold a ping from. The budget it compares against is
// livenessStaleAfter (ten minutes), so a minute is plenty and each tick costs
// one atomic load. A var so tests can drive it.
var livenessSelfExitTick = time.Minute

// livenessUnderSelfExitSupervisor reports whether this provider sits under a
// supervisor that restarts it on exit 75 but has no watchdog of its own, so a
// stall can only be recovered by the provider asking for its own restart:
// OpenRC's supervise-daemon, or a container's start script (which treats 75 as
// a planned restart and loops).
func livenessUnderSelfExitSupervisor() bool {
	kind, restartsOn75 := thrashSupervisorKind(thrashEnvFn, thrashFileExistsFn)
	return restartsOn75 && (kind == "openrc" || kind == "docker")
}

// selfExitStateDurable reports whether the restart throttle can be recorded so
// that it survives the restart. Only a container needs the check: its state
// directory can be a mounted volume that does not survive the restart, and a
// self-exit loop without its throttle record is exactly what the daily ring
// exists to prevent. systemd and OpenRC keep the file on the box itself.
func selfExitStateDurable() error {
	kind, _ := thrashSupervisorKind(thrashEnvFn, thrashFileExistsFn)
	if kind != "docker" {
		return nil
	}
	return thrashCapDirWritableBounded()
}

// runSelfExitLivenessWatchdog is the stall defense for a supervisor that has no
// watchdog. There is no ping to withhold, so the action is the same self-exit
// the thrash watchdog uses (status 75), which the supervisor turns into a
// restart. It is guarded three times over: the durability check above, and the
// same self-heal switch and daily restart budget as the systemd path (through
// gate), so a self-exit can never loop without its throttle record. It leaves
// the same lean start cap behind.
func runSelfExitLivenessWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
	ticker := time.NewTicker(livenessSelfExitTick)
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
				critLog("[liveness] progress resumed; the restart was not taken\n")
			}
			continue
		}
		if acting {
			// This episode already asked for its restart (the exit seam is
			// stubbed in tests). There is nothing further to decide.
			continue
		}
		if err := selfExitStateDurable(); err != nil {
			if !passive {
				passive = true
				critLog("[liveness] no progress for %s, but the state directory holding thrash_cap.json is not writable (%v), so a self-exit would restart without its throttle record; not exiting\n", livenessStaleAfter, err)
			}
			continue
		}
		if ok, why := gate(true); !ok {
			if !passive {
				passive = true
				critLog("[liveness] no progress for %s, but %s, so the provider keeps running (it would otherwise exit %d and be restarted)\n", livenessStaleAfter, why, thrashExitCode)
			}
			continue
		}
		// The throttle record has to be on disk before the exit, or a self-exit
		// could loop without it (the write is bounded, so a failure or a timeout
		// lands here rather than blocking the decision).
		before := len(readThrashCapState().Restarts)
		_ = recordLivenessStall()
		if len(readThrashCapState().Restarts) <= before {
			if !passive {
				passive = true
				critLog("[liveness] no progress for %s, but the restart could not be recorded in thrash_cap.json, so a self-exit would restart without its throttle; not exiting\n", livenessStaleAfter)
			}
			continue
		}
		acting = true
		critLog("🚨 [liveness] no progress for %s: exiting %d so the supervisor restarts the provider\n", livenessStaleAfter, thrashExitCode)
		thrashExitFn(thrashExitCode)
	}
}
