package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// The provider's own cumulative /metrics counters are persisted next to the
// connect-owned ones (see connect/metrics_persist.go) so an upgrade, reboot, or
// crash does not restart them. This file is the adapter: it names the provider
// counters the connect store cannot see and wires the store into startup and
// shutdown.
//
// The hotswap handoff counts as a restart for this purpose: the promoted child
// loads the same store the parent wrote, so the promoted process continues the
// series instead of starting them over.

// metricsCountersStorePath returns ~/.urnetwork/metrics_counters.json, or ""
// when the home directory cannot be resolved (persistence disabled).
func metricsCountersStorePath() string {
	dir := mustStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "metrics_counters.json")
}

// providerMetricCounters is the snapshot the connect store writes for this
// package. Keys are stable: they are the on-disk identity of each series, so
// renaming one resets that counter on the next start.
func providerMetricCounters() map[string]uint64 {
	out := map[string]uint64{
		"control_commands.acked": uint64(max(controlCmdsAcked.Load(), 0)),
		"control_commands.set":   uint64(max(controlCmdsSet.Load(), 0)),
		"control_commands.get":   uint64(max(controlCmdsGet.Load(), 0)),
		"control_commands.clear": uint64(max(controlCmdsClear.Load(), 0)),
	}
	for name, restarts := range snapshotLoopRestarts() {
		out["loop_restarts."+name] = restarts
	}
	return out
}

// restoreProviderMetricCounters adds the stored totals into the provider's
// in-memory counters. Every value is added, never stored: whatever this process
// has already counted stays counted. A second call in the same process is
// ignored, so a duplicate registration cannot double the counters.
func restoreProviderMetricCounters(vals map[string]uint64) {
	providerCountersRestoreMu.Lock()
	if providerCountersRestored {
		providerCountersRestoreMu.Unlock()
		return
	}
	providerCountersRestored = true
	providerCountersRestoreMu.Unlock()

	addCounterTotal(&controlCmdsAcked, vals["control_commands.acked"])
	addCounterTotal(&controlCmdsSet, vals["control_commands.set"])
	addCounterTotal(&controlCmdsGet, vals["control_commands.get"])
	addCounterTotal(&controlCmdsClear, vals["control_commands.clear"])
	for key, v := range vals {
		name, ok := strings.CutPrefix(key, "loop_restarts.")
		if !ok || name == "" {
			continue
		}
		addLoopRestartBase(name, v)
	}
}

// raiseProviderMetricCounters is the catch-up counterpart of
// restoreProviderMetricCounters: it takes each counter to at least the stored
// value instead of adding it, so a HotSwap child picking up the parent's later
// writes cannot double count.
func raiseProviderMetricCounters(vals map[string]uint64) {
	raiseCounterTotal(&controlCmdsAcked, vals["control_commands.acked"])
	raiseCounterTotal(&controlCmdsSet, vals["control_commands.set"])
	raiseCounterTotal(&controlCmdsGet, vals["control_commands.get"])
	raiseCounterTotal(&controlCmdsClear, vals["control_commands.clear"])
	for key, v := range vals {
		name, ok := strings.CutPrefix(key, "loop_restarts.")
		if !ok || name == "" {
			continue
		}
		raiseLoopRestartBase(name, v)
	}
}

var (
	providerCountersRestoreMu sync.Mutex
	providerCountersRestored  bool
	providerMetricsInitOnce   sync.Once
)

// raiseCounterTotal stores v when it is ahead of the counter, clamping at
// MaxInt64. Unlike addCounterTotal it never adds, so it is idempotent.
func raiseCounterTotal(dst *atomic.Int64, v uint64) {
	if v > math.MaxInt64 {
		v = math.MaxInt64
	}
	if cur := dst.Load(); cur < 0 || uint64(cur) < v {
		dst.Store(int64(v))
	}
}

// addCounterTotal adds a stored total to an int64 counter with saturation, so
// a sum past MaxInt64 clamps instead of wrapping negative.
func addCounterTotal(dst *atomic.Int64, v uint64) {
	if v == 0 {
		return
	}
	for {
		cur := dst.Load()
		if cur >= math.MaxInt64 {
			return
		}
		next := int64(math.MaxInt64)
		if v <= uint64(math.MaxInt64-cur) {
			next = cur + int64(v)
		}
		if dst.CompareAndSwap(cur, next) {
			return
		}
	}
}

// initMetricsCountersPersistence registers the provider's counters and restores
// the store before the /metrics listener can serve a scrape. startFlush is
// false for a HotSwap candidate: while the parent is still serving, the
// candidate must not become a second writer. The flush starts at takeover, see
// startMetricsCountersAfterTakeover. Safe to call more than once.
func initMetricsCountersPersistence(startFlush bool) {
	providerMetricsInitOnce.Do(func() {
		connect.RegisterPersistentCounterSource(connect.PersistentCounterSource{
			Name:     "provider",
			Snapshot: providerMetricCounters,
			Restore:  restoreProviderMetricCounters,
			Raise:    raiseProviderMetricCounters,
		})
		path := metricsCountersStorePath()
		if path == "" {
			tlog("[metrics] counter persistence disabled: no provider state directory\n")
			return
		}
		if err := connect.RestoreMetricsCounters(path); err != nil {
			tlog("[metrics] counter restore skipped: %v\n", err)
		}
		tlog("[metrics] cumulative counters persist across restarts in %s\n", path)
	})
	if !startFlush {
		return
	}
	if path := metricsCountersStorePath(); path != "" {
		connect.StartMetricsCounters(path, connect.DefaultMetricsCountersFlushInterval)
	}
}

// startMetricsCountersAfterTakeover makes a promoted HotSwap child the store's
// writer: it catches up to whatever the draining parent wrote after the child's
// own restore, then starts the periodic flush. The parent flushes once more at
// the end of its drain, which is after this takeover, so a second catch-up runs
// once the drain window has passed and picks up those final numbers.
func startMetricsCountersAfterTakeover(ctx context.Context) {
	path := metricsCountersStorePath()
	if path == "" {
		return
	}
	if err := connect.CatchUpMetricsCounters(); err != nil {
		tlog("[metrics] counter catch-up after takeover failed: %v\n", err)
	}
	connect.StartMetricsCounters(path, connect.DefaultMetricsCountersFlushInterval)

	go connect.HandleError(func() {
		timer := time.NewTimer(HotSwapDrainTimeout + auditReconcileGrace)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := connect.CatchUpMetricsCounters(); err != nil {
			tlog("[metrics] delayed counter catch-up after takeover failed: %v\n", err)
		}
	})
}

// flushMetricsCountersOnExit stops the periodic writer and writes the final
// totals. Called from the shutdown path, before os.Exit, where defers do not
// run.
func flushMetricsCountersOnExit() {
	connect.StopMetricsCounters()
	if err := connect.FlushMetricsCounters(); err != nil {
		tlog("[metrics] final counter flush failed: %v\n", err)
	}
}
