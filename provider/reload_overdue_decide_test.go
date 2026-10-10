package main

import (
	"testing"
	"time"
)

// Golden table for reloadOverdueDecide. The fire/held outcomes replay the
// states driven end to end through RunReloadWatchdog before the extraction
// (capture pass: /tmp/mst/p2/reload-decide.pre-extraction.txt); the loop-level
// behavior stays pinned by TestNowFnSeamDrivesReloadWatchdog and the
// TestReloadWatchdog* family.
func TestReloadOverdueDecide(t *testing.T) {
	oldLimit, oldReFire := reloadHardLimit, reloadWatchdogReFire
	t.Cleanup(func() { reloadHardLimit, reloadWatchdogReFire = oldLimit, oldReFire })
	reloadHardLimit = 20 * time.Minute
	reloadWatchdogReFire = 5 * time.Minute

	t0 := time.Unix(1_800_000_000, 0)
	startedAgo := func(d time.Duration) int64 { return t0.Add(-d).UnixNano() }

	tests := []struct {
		name         string
		now          time.Time
		startedAt    int64
		active       bool
		lastFired    time.Time
		episodeStart int64
		wantFire     bool
		wantHeld     time.Duration
	}{
		// No active reload: no escalation and no held duration.
		{"not-active", t0, startedAgo(21 * time.Minute), false, time.Time{}, 0, false, 0},
		// Active but without a live timestamp: the watchdog waits.
		{"no-timestamp", t0, 0, true, time.Time{}, 0, false, 0},
		// Within the hard limit.
		{"within-limit", t0, startedAgo(19 * time.Minute), true, time.Time{}, 0, false, 19 * time.Minute},
		// At the limit: the comparison is held < limit, so exactly 20m fires.
		{"at-limit-inclusive", t0, startedAgo(20 * time.Minute), true, time.Time{}, 0, true, 20 * time.Minute},
		{"overdue", t0, startedAgo(21 * time.Minute), true, time.Time{}, 0, true, 21 * time.Minute},
		// Same episode, one minute after the first fire: throttled.
		{"same-episode-within-refire", t0.Add(time.Minute), startedAgo(21 * time.Minute), true, t0, startedAgo(21 * time.Minute), false, 22 * time.Minute},
		// Same episode, six minutes after the first fire (past the 5m throttle).
		{"same-episode-past-refire", t0.Add(6 * time.Minute), startedAgo(21 * time.Minute), true, t0, startedAgo(21 * time.Minute), true, 27 * time.Minute},
		// A fresh episode fires immediately even though the last fire is
		// recent: the throttle is per episode (episodeStart was reset).
		{"new-episode-fires-immediately", t0.Add(30 * time.Minute), t0.Add(5 * time.Minute).UnixNano(), true, t0, 0, true, 25 * time.Minute},
		// Just under the hard limit: the comparison is held < limit, so a
		// nanosecond short still does not fire.
		{"just-under-limit", t0, startedAgo(20*time.Minute - time.Nanosecond), true, time.Time{}, 0, false, 20*time.Minute - time.Nanosecond},
		// A negative timestamp never fires.
		{"negative-startedAt", t0, -1, true, time.Time{}, 0, false, 0},
		// A backward clock step (startedAt in the future) yields a negative
		// held, which cannot fire.
		{"clock-skew-backwards", t0, startedAgo(-5 * time.Minute), true, time.Time{}, 0, false, -5 * time.Minute},
		// At EXACTLY the re-fire window on the same episode the comparison
		// is strict (<), so it fires.
		{"refire-boundary-exact", t0.Add(5 * time.Minute), startedAgo(21 * time.Minute), true, t0, startedAgo(21 * time.Minute), true, 26 * time.Minute},
		// A different, non-zero episode is not throttled by a recent fire:
		// without the episodeStart == startedAt clause this row fails.
		{"different-nonzero-episode-recent-fire", t0.Add(time.Minute), startedAgo(21 * time.Minute), true, t0, startedAgo(41 * time.Minute), true, 22 * time.Minute},
	}

	for _, tt := range tests {
		gotFire, gotHeld := reloadOverdueDecide(tt.now, tt.startedAt, tt.active, tt.lastFired, tt.episodeStart)
		if gotFire != tt.wantFire || gotHeld != tt.wantHeld {
			t.Errorf("%s: reloadOverdueDecide() = (fire=%v, held=%v), want (%v, %v)",
				tt.name, gotFire, gotHeld, tt.wantFire, tt.wantHeld)
		}
	}
}
