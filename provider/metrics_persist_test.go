package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// snapshotProviderCounters saves the provider counters this package persists
// and restores them when the test ends.
func snapshotProviderCounters(t *testing.T) {
	t.Helper()
	acked, set, get, clear := controlCmdsAcked.Load(), controlCmdsSet.Load(), controlCmdsGet.Load(), controlCmdsClear.Load()
	loopStatusMu.Lock()
	statuses := make(map[string]loopStatus, len(loopStatuses))
	for name, st := range loopStatuses {
		statuses[name] = *st
	}
	base := make(map[string]int64, len(loopRestartBase))
	for name, v := range loopRestartBase {
		base[name] = v
	}
	loopStatusMu.Unlock()

	t.Cleanup(func() {
		controlCmdsAcked.Store(acked)
		controlCmdsSet.Store(set)
		controlCmdsGet.Store(get)
		controlCmdsClear.Store(clear)
		loopStatusMu.Lock()
		loopStatuses = map[string]*loopStatus{}
		for name, st := range statuses {
			copied := st
			loopStatuses[name] = &copied
		}
		loopRestartBase = base
		loopStatusMu.Unlock()
	})

	controlCmdsAcked.Store(0)
	controlCmdsSet.Store(0)
	controlCmdsGet.Store(0)
	controlCmdsClear.Store(0)
	loopStatusMu.Lock()
	loopStatuses = map[string]*loopStatus{}
	loopRestartBase = map[string]int64{}
	loopStatusMu.Unlock()
}

func TestProviderMetricCountersSnapshotShape(t *testing.T) {
	snapshotProviderCounters(t)

	IncrControlCmd("get")
	IncrControlCmd("get")
	IncrControlCmd("set")
	addLoopRestartBase("pressure_monitor", 3)
	setLoopUp("pressure_monitor", false, true) // one live restart

	got := providerMetricCounters()
	want := map[string]uint64{
		"control_commands.acked":         3,
		"control_commands.get":           2,
		"control_commands.set":           1,
		"control_commands.clear":         0,
		"loop_restarts.pressure_monitor": 4, // 3 persisted + 1 live
	}
	for key, wantV := range want {
		if got[key] != wantV {
			t.Errorf("%s = %d, want %d", key, got[key], wantV)
		}
	}
}

func TestProviderMetricCountersRestoreAddsToLiveValues(t *testing.T) {
	snapshotProviderCounters(t)

	// The counters may already have moved by the time the store is applied;
	// the restore must add, never overwrite.
	IncrControlCmd("clear")

	restoreProviderMetricCounters(map[string]uint64{
		"control_commands.acked":        10,
		"control_commands.clear":        4,
		"loop_restarts.pool_controller": 7,
	})

	if got := controlCmdsAcked.Load(); got != 11 {
		t.Errorf("acked = %d, want 11 (1 live + 10 restored)", got)
	}
	if got := controlCmdsClear.Load(); got != 5 {
		t.Errorf("clear = %d, want 5 (1 live + 4 restored)", got)
	}
	// The loop has no live status yet, so it is not exported as a series, but
	// the restored base must survive in the snapshot the store writes (a flush
	// before the loop starts must not drop the previous process's count).
	if snap := snapshotLoopRestarts(); snap["pool_controller"] != 7 {
		t.Errorf("snapshot loop restarts = %d, want 7", snap["pool_controller"])
	}
	if metrics := supervisedLoopMetrics(); strings.Contains(metrics, "pool_controller") {
		t.Errorf("a loop that has not started must not be exported yet:\n%s", metrics)
	}
	// Once the loop starts, the series continues from the restored base.
	loopStatusOf("pool_controller")
	if metrics := supervisedLoopMetrics(); !strings.Contains(metrics, `urnet_loop_restarts_total{loop="pool_controller"} 7`) {
		t.Errorf("restored loop base not exported after registration:\n%s", metrics)
	}
}

func TestLoopRestartTotalAddsBaseToLive(t *testing.T) {
	snapshotProviderCounters(t)

	addLoopRestartBase("degraded_proxy_reaper", 5)
	setLoopUp("degraded_proxy_reaper", true, false)
	setLoopUp("degraded_proxy_reaper", false, true)
	setLoopUp("degraded_proxy_reaper", true, false)
	setLoopUp("degraded_proxy_reaper", false, true)

	metrics := supervisedLoopMetrics()
	if !strings.Contains(metrics, `urnet_loop_restarts_total{loop="degraded_proxy_reaper"} 7`) {
		t.Errorf("restart total = base(5) + live(2) expected:\n%s", metrics)
	}
	if snap := snapshotLoopRestarts(); snap["degraded_proxy_reaper"] != 7 {
		t.Errorf("snapshot loop restarts = %d, want 7", snap["degraded_proxy_reaper"])
	}
}

func TestLoopRestartTotalSaturates(t *testing.T) {
	snapshotProviderCounters(t)

	addLoopRestartBase("saturating_loop", math.MaxInt64)
	setLoopUp("saturating_loop", true, false)
	setLoopUp("saturating_loop", false, true)
	if got := snapshotLoopRestarts()["saturating_loop"]; got != math.MaxInt64 {
		t.Errorf("restart total = %d, want saturation at MaxInt64", got)
	}
}

func TestAddCounterTotalSaturates(t *testing.T) {
	var c atomic.Int64
	c.Store(math.MaxInt64 - 3)
	addCounterTotal(&c, 10)
	if got := c.Load(); got != math.MaxInt64 {
		t.Fatalf("saturated value = %d, want %d", got, int64(math.MaxInt64))
	}
}

func TestRestoreProviderMetricCountersIsIdempotent(t *testing.T) {
	snapshotProviderCounters(t)

	// The restore latch is per process; reset it for this test.
	providerCountersRestoreMu.Lock()
	providerCountersRestored = false
	providerCountersRestoreMu.Unlock()
	t.Cleanup(func() {
		providerCountersRestoreMu.Lock()
		providerCountersRestored = false
		providerCountersRestoreMu.Unlock()
	})

	vals := map[string]uint64{"control_commands.acked": 4, "loop_restarts.pressure_monitor": 2}
	restoreProviderMetricCounters(vals)
	restoreProviderMetricCounters(vals)

	if got := controlCmdsAcked.Load(); got != 4 {
		t.Errorf("acked = %d, want 4 (a repeated restore must not double)", got)
	}
	if got := snapshotLoopRestarts()["pressure_monitor"]; got != 2 {
		t.Errorf("loop restarts = %d, want 2", got)
	}
}

func TestRaiseProviderMetricCountersDoesNotDouble(t *testing.T) {
	snapshotProviderCounters(t)

	controlCmdsAcked.Store(7)
	// A lower stored value must not lower the live counter.
	raiseProviderMetricCounters(map[string]uint64{"control_commands.acked": 3})
	if got := controlCmdsAcked.Load(); got != 7 {
		t.Errorf("acked = %d, want 7 (raise must not lower)", got)
	}
	raiseProviderMetricCounters(map[string]uint64{"control_commands.acked": 9})
	if got := controlCmdsAcked.Load(); got != 9 {
		t.Errorf("acked = %d, want 9", got)
	}
	// Repeating the same catch-up is idempotent, which is the property the
	// delayed post-drain catch-up relies on.
	raiseProviderMetricCounters(map[string]uint64{"control_commands.acked": 9})
	if got := controlCmdsAcked.Load(); got != 9 {
		t.Errorf("acked = %d after a repeated raise, want 9", got)
	}

	raiseLoopRestartBase("catchup_loop", 4)
	raiseLoopRestartBase("catchup_loop", 4)
	if got := snapshotLoopRestarts()["catchup_loop"]; got != 4 {
		t.Errorf("loop restarts = %d, want 4 (raise, not add)", got)
	}
}

func TestMetricsCountersStorePath(t *testing.T) {
	path := metricsCountersStorePath()
	if path == "" {
		t.Skip("no state directory resolvable in this environment")
	}
	if !strings.HasSuffix(path, "/.urnetwork/metrics_counters.json") {
		t.Errorf("store path = %q, want under ~/.urnetwork", path)
	}
	if !strings.Contains(path, "metrics_counters.json") {
		t.Errorf("store path = %q", path)
	}
	// The lifetime ledger must stay a separate file: mixing the two stores
	// would double count the billable bytes the ledger already accumulates.
	if strings.Contains(path, "lifetime_metrics") {
		t.Error("the counter store must not reuse lifetime_metrics.json")
	}
}

// TestInitMetricsCountersPersistenceWritesStore drives the wiring main uses:
// register the provider counters, restore from the state directory, flush. It
// checks the on-disk JSON shape (a contract the next process reads) and that
// the provider's own counters reach it.
func TestInitMetricsCountersPersistenceWritesStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	IncrControlCmd("get")
	initMetricsCountersPersistence(true)
	// initMetricsCountersPersistence starts the periodic writer; stop it before
	// the final flush so the test owns the last write.
	flushMetricsCountersOnExit()

	path := filepath.Join(home, ".urnetwork", "metrics_counters.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("store not written: %v", err)
	}
	var file struct {
		Version          int                          `json:"version"`
		PID              int                          `json:"pid"`
		ProcessStartUnix int64                        `json:"process_start_unix"`
		Extras           map[string]map[string]uint64 `json:"extras"`
		Counters         map[string]json.RawMessage   `json:"counters"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("store does not parse: %v", err)
	}
	if file.Version != 1 {
		t.Errorf("version = %d, want 1", file.Version)
	}
	if file.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", file.PID, os.Getpid())
	}
	if file.ProcessStartUnix == 0 {
		t.Error("process_start_unix not written")
	}
	if got := file.Extras["provider"]["control_commands.get"]; got < 1 {
		t.Errorf("control_commands.get = %d, want >= 1", got)
	}
	if _, ok := file.Counters["h1"]; !ok {
		t.Errorf("counters block missing h1: %v", file.Counters)
	}
}
