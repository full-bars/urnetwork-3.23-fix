package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Supervision for the background control loops (pressure monitor, pool
// controller, degraded-proxy reaper). They used to run under a bare
// connect.HandleError: a panic logged one line and ended the loop for good.
// A dead pressure monitor leaves the last score, the tightened GOGC and the
// shrunk memory budget in force, and a high last score keeps the pool
// controller cutting to its floor. Nothing noticed.
const (
	loopBackoffBase = time.Second
	loopBackoffMax  = 5 * time.Minute
	// loopHealthyRun is how long a run must last before the backoff resets, so
	// a loop that works for a while and then fails starts again quickly.
	loopHealthyRun = 10 * time.Minute
)

// loopSleep waits d or until ctx ends; false means ctx ended. A seam for tests.
var loopSleep = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// loopStatus is what the metrics report about one supervised loop.
type loopStatus struct {
	Up       bool
	Restarts int64
}

var (
	loopStatusMu sync.Mutex
	loopStatuses = map[string]*loopStatus{}
)

// loopStatusOf returns a copy of the named loop's status, registering it.
func loopStatusOf(name string) loopStatus {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	return *loopStatusLocked(name)
}

func loopStatusLocked(name string) *loopStatus {
	st, ok := loopStatuses[name]
	if !ok {
		st = &loopStatus{}
		loopStatuses[name] = st
	}
	return st
}

func setLoopUp(name string, up bool, restart bool) {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	st := loopStatusLocked(name)
	st.Up = up
	if restart {
		st.Restarts++
	}
}

// nextLoopBackoff doubles the previous backoff up to loopBackoffMax, and
// resets it after a run that lasted loopHealthyRun.
func nextLoopBackoff(prev, ranFor time.Duration) time.Duration {
	if prev <= 0 || ranFor >= loopHealthyRun {
		return loopBackoffBase
	}
	if next := prev * 2; next < loopBackoffMax {
		return next
	}
	return loopBackoffMax
}

// superviseLoop runs fn until ctx ends, restarting it with backoff when it
// panics or returns while ctx is still live. onFail (optional) runs after
// every failed run, before the backoff, so the loop's actuators can be put
// back to neutral instead of staying wherever the dead loop left them.
func superviseLoop(ctx context.Context, name string, fn func(), onFail func()) {
	var backoff time.Duration
	loopStatusOf(name) // register so the series exists before the first failure
	for {
		started := time.Now()
		setLoopUp(name, true, false)
		connect.HandleError(fn)
		setLoopUp(name, false, false)
		if ctx.Err() != nil {
			return
		}
		backoff = nextLoopBackoff(backoff, time.Since(started))
		tlog("[proxy][supervisor] loop %s ended unexpectedly, restarting in %s\n", name, backoff)
		if onFail != nil {
			connect.HandleError(onFail)
		}
		setLoopUp(name, false, true)
		if !loopSleep(ctx, backoff) {
			return
		}
	}
}

// supervisedLoopMetrics renders the per-loop Prometheus series.
func supervisedLoopMetrics() string {
	loopStatusMu.Lock()
	names := make([]string, 0, len(loopStatuses))
	for name := range loopStatuses {
		names = append(names, name)
	}
	sort.Strings(names)
	snap := make(map[string]loopStatus, len(names))
	for _, name := range names {
		snap[name] = *loopStatuses[name]
	}
	loopStatusMu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP urnet_loop_restarts_total Times a supervised background loop ended unexpectedly and was restarted.\n")
	b.WriteString("# TYPE urnet_loop_restarts_total counter\n")
	for _, name := range names {
		fmt.Fprintf(&b, "urnet_loop_restarts_total{loop=%q} %d\n", name, snap[name].Restarts)
	}
	b.WriteString("# HELP urnet_loop_up 1 while a supervised background loop is running, 0 while it is stopped or backing off.\n")
	b.WriteString("# TYPE urnet_loop_up gauge\n")
	for _, name := range names {
		up := 0
		if snap[name].Up {
			up = 1
		}
		fmt.Fprintf(&b, "urnet_loop_up{loop=%q} %d\n", name, up)
	}
	return b.String()
}

// resetPressureActuators puts everything the pressure monitor drives back to
// neutral: the published score, the connection memory budget, and a GOGC the
// governor had tightened. setGC is debug.SetGCPercent in production. Run when
// the monitor exits for any reason, so a dead monitor fails neutral instead of
// freezing the last (possibly emergency) reading in force.
func resetPressureActuators(state *gcGovernorState, setGC func(int) int) {
	setPressure(0)
	applyPressureMemoryBudget(0)
	gcTightening.Store(false)
	if state != nil {
		if state.currentGOGC != state.baselineGOGC {
			setGC(state.baselineGOGC)
			state.currentGOGC = state.baselineGOGC
		}
		state.level = 0
		state.consecutiveCalmCount = 0
	}
}
