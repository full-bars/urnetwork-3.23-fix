package main

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// A systemd watchdog the provider cannot satisfy while it is stalled.
//
// The failure it exists for: the Go runtime on a small box can spend nearly all
// of its time in the garbage collector (heap above its soft limit, part of it
// swapped out), so the process stays alive at 50 to 90 percent CPU while every
// goroutine, the self-heal loops included, makes no useful progress. systemd
// sees a running process, the thrash watchdog sees no sustained pressure
// reading, and nothing ever restarts it. An in-process responder cannot fix
// that, because it shares the starved runtime; a supervisor outside the process
// can. With WatchdogSec set, systemd kills and restarts a unit that stops
// sending WATCHDOG=1, so the provider sends it only while the loop that proves
// the process can still work keeps ticking.
//
// It is inert unless systemd sets WATCHDOG_USEC (a unit with WatchdogSec=).

// livenessStaleAfter is how long the progress loop may stay silent before the
// ping is withheld. The pressure monitor ticks every 30 seconds, so this is
// twenty missed ticks: slow enough that a long GC pause or a briefly loaded box
// never trips it (a false restart costs a few minutes of earnings), fast enough
// that a wedged process is replaced in well under an hour.
const livenessStaleAfter = 10 * time.Minute

// livenessRecordTimeout bounds the lean-cap write: a blocked lock must not
// delay the watchdog's decision.
const livenessRecordTimeout = 5 * time.Second

// livenessProgress records when the process last proved it can make progress.
type livenessProgress struct {
	nowFn func() time.Time
	nano  atomic.Int64
}

func newLivenessProgress(nowFn func() time.Time) *livenessProgress {
	l := &livenessProgress{nowFn: nowFn}
	l.note() // starting up is progress
	return l
}

// note records progress now.
func (self *livenessProgress) note() {
	self.nano.Store(self.nowFn().UnixNano())
}

// fresh reports whether progress was recorded within staleAfter.
func (self *livenessProgress) fresh(staleAfter time.Duration) bool {
	return self.nowFn().Sub(time.Unix(0, self.nano.Load())) <= staleAfter
}

// processLiveness is the one the pressure monitor feeds.
var processLiveness = newLivenessProgress(time.Now)

// noteLivenessProgress is called by the loop whose ticking proves the process
// can still schedule goroutines, take its locks and write its logs.
func noteLivenessProgress() {
	processLiveness.note()
}

// sdWatchdogInterval is how often to ping, derived from systemd's WATCHDOG_USEC:
// a third of the timeout, never under one second. ok is false when the watchdog
// is not enabled for this process.
//
// WATCHDOG_PID is deliberately ignored. sd_watchdog_enabled() disables the
// watchdog in a process whose pid differs, but a hot-swap candidate becomes the
// main pid of the same unit and must keep the watchdog fed.
func sdWatchdogInterval(getenv func(string) string) (time.Duration, bool) {
	usec, err := strconv.ParseInt(getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	interval := time.Duration(usec) * time.Microsecond / 3
	return max(interval, time.Second), true
}

// runSdWatchdogLoop pings on every tick while progress is fresh. While it is
// not, it withholds the ping, logs once per episode and runs onStall once; the
// undo onStall returns runs if progress resumes before systemd acts. When acting
// reports false (self-heal is off) nothing is withheld or recorded: the ping
// keeps flowing and the line says what would have happened, because off means
// off for automatic restarts.
func runSdWatchdogLoop(ctx context.Context, tick <-chan time.Time, progress *livenessProgress, staleAfter time.Duration, ping func() error, logf func(string, ...any), acting func() bool, onStall func() func()) {
	stalled := false
	var undo func()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		if progress.fresh(staleAfter) {
			if stalled {
				stalled = false
				if undo != nil {
					undo()
					undo = nil
				}
				logf("[liveness] progress resumed; the systemd watchdog is being fed again\n")
			}
			_ = ping()
			continue
		}
		if !acting() {
			if !stalled {
				stalled = true
				logf("[liveness] no progress for %s, but self-heal is off, so the systemd watchdog is still being fed (it would otherwise be withheld and systemd would restart the provider)\n", staleAfter)
			}
			_ = ping()
			continue
		}
		if !stalled {
			stalled = true
			logf("🚨 [liveness] no progress for %s: withholding the systemd watchdog ping, so systemd will restart the provider\n", staleAfter)
			if onStall != nil {
				undo = onStall()
			}
		}
	}
}

// runSdWatchdog starts the watchdog when systemd asked for one. It logs through
// the disk event log, not the ramlog pipe: the ramlog reader may be the thing
// that is starved, and a log call that blocks must not stop the ping decision.
// acting says whether automatic restarts are allowed (the self-heal switch).
func runSdWatchdog(ctx context.Context, acting func() bool) {
	interval, ok := sdWatchdogInterval(os.Getenv)
	if !ok {
		return
	}
	if os.Getenv("NOTIFY_SOCKET") == "" {
		// WatchdogSec= is set but systemd gave this process no notify socket
		// (NotifyAccess=none), so no ping can ever reach it and systemd will
		// restart the unit every interval whatever this process does.
		critLog("⚠️ [liveness] WATCHDOG_USEC is set but NOTIFY_SOCKET is not: the unit needs NotifyAccess=main or all, or systemd will restart the provider every %s\n", interval*3)
		return
	}
	// start from a clean slate: the startup time is progress
	processLiveness.note()
	_ = notifySystemdWatchdog() // first feed right away
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runSdWatchdogLoop(ctx, ticker.C, processLiveness, livenessStaleAfter, notifySystemdWatchdog, critLog, acting, recordLivenessStall)
}

// recordLivenessStall leaves the next start leaner before systemd restarts a
// stalled provider. A stall of this kind is what an over-full box looks like
// from inside, and starting again with the same proxy count walks straight into
// it, so it is recorded exactly like a thrash escape: a cap of a fraction of
// what was running, and an entry in the anti-loop ring that the daily ceiling
// counts. The write is bounded; a blocked lock must not delay the decision.
// The returned undo restores the previous state, for a stall that resolves
// itself before systemd acts: that must not leave a cap or spend the daily ring.
func recordLivenessStall() func() {
	now := time.Now()
	prev := readThrashCapState()
	cap := thrashCapForNextStart(int(lastRunningProxyCount.Load()))
	done := make(chan error, 1)
	go func() { done <- recordThrashEscalation(cap, now) }()
	select {
	case err := <-done:
		if err != nil {
			critLog("[liveness] could not record the lean start cap: %v\n", err)
			return nil
		}
		if cap > 0 {
			critLog("[liveness] the next start is capped at %d proxies (60%% of what was running)\n", cap)
		}
	case <-time.After(livenessRecordTimeout):
		critLog("[liveness] timed out recording the lean start cap\n")
		return nil
	}
	return func() {
		path, err := thrashCapPath()
		if err != nil {
			return
		}
		if err := oomWriteJSON(path, prev); err != nil {
			critLog("[liveness] could not restore the start cap after progress resumed: %v\n", err)
			return
		}
		critLog("[liveness] the lean start cap was removed again: progress resumed before systemd acted\n")
	}
}
