package main

import (
	"context"
	"fmt"
	"math"
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
	// loopRestartBase is the restart count the previous process persisted per
	// loop (see provider/metrics_persist.go). The exported counter is
	// base + live, so urnet_loop_restarts_total keeps counting up across a
	// provider restart instead of falling back to zero. Guarded by loopStatusMu.
	loopRestartBase = map[string]int64{}
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

// addLoopRestartBase seeds a loop's restart count from the persisted store.
// The name may not have a live status yet (the loop is registered when its
// goroutine first starts), so the base map is independent of loopStatuses.
func addLoopRestartBase(name string, v uint64) {
	if name == "" || v == 0 {
		return
	}
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	loopRestartBase[name] = satAddInt64(loopRestartBase[name], v)
}

// raiseLoopRestartBase takes a loop's persisted restart count to at least v.
// The catch-up path uses it instead of addLoopRestartBase so picking up the
// parent's later write after a HotSwap cannot double count.
func raiseLoopRestartBase(name string, v uint64) {
	if name == "" || v == 0 {
		return
	}
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	if v > uint64(max(loopRestartBase[name], 0)) {
		loopRestartBase[name] = satAddInt64(0, v)
	}
}

// loopRestartTotalLocked returns base + live for one loop, saturating at
// MaxInt64 instead of wrapping negative. Caller holds loopStatusMu.
func loopRestartTotalLocked(name string) int64 {
	var live int64
	if st, ok := loopStatuses[name]; ok {
		live = max(st.Restarts, 0)
	}
	return satAddInt64(loopRestartBase[name], uint64(live))
}

func satAddInt64(a int64, b uint64) int64 {
	if b > math.MaxInt64 || a > math.MaxInt64-int64(b) {
		return math.MaxInt64
	}
	return a + int64(b)
}

// snapshotLoopRestarts returns the exported restart total per loop, covering
// loops that have a live status and loops the previous process counted but
// this one has not started yet.
func snapshotLoopRestarts() map[string]uint64 {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	out := make(map[string]uint64, len(loopStatuses)+len(loopRestartBase))
	for name := range loopStatuses {
		out[name] = uint64(max(loopRestartTotalLocked(name), 0))
	}
	for name := range loopRestartBase {
		if _, ok := out[name]; ok {
			continue
		}
		out[name] = uint64(max(loopRestartTotalLocked(name), 0))
	}
	return out
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
		if !loopSleep(ctx, backoff) || ctx.Err() != nil {
			return
		}
		// Counted only once another run is about to start, so a shutdown
		// during the backoff does not report a restart that never happened.
		setLoopUp(name, false, true)
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
	restarts := make(map[string]int64, len(names))
	for _, name := range names {
		snap[name] = *loopStatuses[name]
		restarts[name] = loopRestartTotalLocked(name)
	}
	loopStatusMu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP urnet_loop_restarts_total Times a supervised background loop ended unexpectedly and was restarted.\n")
	b.WriteString("# TYPE urnet_loop_restarts_total counter\n")
	for _, name := range names {
		fmt.Fprintf(&b, "urnet_loop_restarts_total{loop=%q} %d\n", name, restarts[name])
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
	setPressureNoCPU(0)
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
