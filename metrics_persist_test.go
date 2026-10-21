package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// saveCounters snapshots the cumulative counters this package persists and
// restores them when the test ends, so one test's traffic never leaks into
// another's assertions. The atomics are Stored, never assigned: copying an
// atomic value trips the copylocks vet check and races concurrent readers.
func saveCounters(t *testing.T) {
	t.Helper()
	type modeSnap struct{ tx, rx, btx, brx uint64 }
	saveMode := func(s *transportModeStats) modeSnap {
		return modeSnap{s.framesTx.Load(), s.framesRx.Load(), s.bytesTx.Load(), s.bytesRx.Load()}
	}
	restoreMode := func(s *transportModeStats, m modeSnap) {
		s.framesTx.Store(m.tx)
		s.framesRx.Store(m.rx)
		s.bytesTx.Store(m.btx)
		s.bytesRx.Store(m.brx)
	}
	type h3Snap struct {
		modeSnap
		attempts, connects, failures, drops uint64
	}
	saveH3 := func(c *h3FamilyCounters) h3Snap {
		return h3Snap{
			modeSnap: saveMode(&c.transportModeStats),
			attempts: c.attempts.Load(),
			connects: c.connects.Load(),
			failures: c.connectFailures.Load(),
			drops:    c.drops.Load(),
		}
	}
	restoreH3 := func(c *h3FamilyCounters, s h3Snap) {
		restoreMode(&c.transportModeStats, s.modeSnap)
		c.attempts.Store(s.attempts)
		c.connects.Store(s.connects)
		c.connectFailures.Store(s.failures)
		c.drops.Store(s.drops)
	}

	h1 := saveMode(&h1ModeStats)
	h1d := saveMode(&h1DirectStats)
	h3 := saveH3(&h3Counters)
	h3dns := saveH3(&h3DnsCounters)
	h3pump := saveH3(&h3DnsPumpCounters)
	dg := snapshotDatagramCounters()

	globalProm.initErrors()
	errs := map[string]int64{}
	for cat, c := range globalProm.errorsTotal {
		errs[string(cat)] = c.Load()
	}
	contractsIn, contractsOut := globalProm.contractsIn.Load(), globalProm.contractsOut.Load()
	doh := dohFailureTotal.Load()
	dohInterval := dohFailureCount.Load()

	// The fixture is a clean slate, not just a restore point: these counters
	// are process globals, and tests that ran earlier in the package (the
	// error tracking tests leave every category at 5) otherwise inflate the
	// absolute values the persistence tests assert.
	zeroCounters(t)

	t.Cleanup(func() {
		restoreMode(&h1ModeStats, h1)
		restoreMode(&h1DirectStats, h1d)
		restoreH3(&h3Counters, h3)
		restoreH3(&h3DnsCounters, h3dns)
		restoreH3(&h3DnsPumpCounters, h3pump)
		zeroDatagramCounters()
		seedDatagramCounters(dg)
		for cat, c := range globalProm.errorsTotal {
			c.Store(errs[string(cat)])
		}
		globalProm.contractsIn.Store(contractsIn)
		globalProm.contractsOut.Store(contractsOut)
		dohFailureTotal.Store(doh)
		dohFailureCount.Store(dohInterval)
	})
}

// zeroCounters clears every persisted counter so a simulated restart starts
// from zero, exactly as a fresh process does.
func zeroCounters(t *testing.T) {
	t.Helper()
	for _, s := range []*transportModeStats{&h1ModeStats, &h1DirectStats} {
		s.framesTx.Store(0)
		s.framesRx.Store(0)
		s.bytesTx.Store(0)
		s.bytesRx.Store(0)
	}
	for _, c := range []*h3FamilyCounters{&h3Counters, &h3DnsCounters, &h3DnsPumpCounters} {
		c.framesTx.Store(0)
		c.framesRx.Store(0)
		c.bytesTx.Store(0)
		c.bytesRx.Store(0)
		c.attempts.Store(0)
		c.connects.Store(0)
		c.connectFailures.Store(0)
		c.drops.Store(0)
	}
	zeroDatagramCounters()
	globalProm.initErrors()
	for _, c := range globalProm.errorsTotal {
		c.Store(0)
	}
	globalProm.contractsIn.Store(0)
	globalProm.contractsOut.Store(0)
	dohFailureTotal.Store(0)
	dohFailureCount.Store(0)
}

func zeroDatagramCounters() {
	h3DatagramOffered.Store(0)
	h3DatagramAccepted.Store(0)
	h3DatagramDrops.Store(0)
	h3DatagramBlackholes.Store(0)
	h3DatagramStats.receivedMessageCount.Store(0)
	h3DatagramStats.receivedMessageByteCount.Store(0)
	h3DatagramStats.malformedFragmentCount.Store(0)
	h3DatagramStats.duplicateFragmentCount.Store(0)
	h3DatagramStats.checksumFailureCount.Store(0)
	h3DatagramStats.sentMessageCount.Store(0)
	h3DatagramStats.sentMessageByteCount.Store(0)
	h3DatagramStats.streamSentMessageCount.Store(0)
	h3DatagramStats.sendErrorCount.Store(0)
}

// resetMetricsPersist clears the in-memory half of the store so a test can run
// RestoreMetricsCounters more than once in one process, the way separate
// provider processes would.
func resetMetricsPersist(t *testing.T) {
	t.Helper()
	StopMetricsCounters()
	metricsPersistMu.Lock()
	metricsPersistPath = ""
	metricsRestored = false
	metricsProxyBase = map[string]metricsProxyCounters{}
	metricsPersistMu.Unlock()
	persistentSourcesMu.Lock()
	persistentSources = map[string]PersistentCounterSource{}
	pendingSourceTotals = map[string]map[string]uint64{}
	persistentSourcesMu.Unlock()
}

func testStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "metrics_counters.json")
}

// registerTestProxyWithTraffic registers a proxy and returns its bandwidth
// record, unregistering it when the test ends. The unregister also folds the
// final totals into the store, which is what a real churn does.
func registerTestProxyWithTraffic(t *testing.T, index int, key string, rx, billableTx uint64) *ProxyBandwidth {
	t.Helper()
	RegisterProxy(index, key, key)
	bw := RegisterProxyBandwidth(index)
	bw.TotalRx.Add(rx)
	bw.BillableTx.Add(billableTx)
	t.Cleanup(func() { UnregisterProxy(index) })
	return bw
}

func TestMetricsCountersSurviveRestart(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore on a missing file: %v", err)
	}

	IncrError(ErrorTransport)
	IncrError(ErrorTransport)
	IncrError(ErrorProxy)
	IncrContractAcquired()
	IncrContractAcquired()
	IncrContractDenied()
	h1ModeStats.framesTx.Add(3)
	h1ModeStats.bytesTx.Add(300)
	h3Counters.attempts.Add(2)
	h3Counters.connects.Add(1)
	h3DatagramOffered.Add(4)
	h3DatagramStats.sentMessageCount.Add(9)
	h3DatagramStats.sentMessageByteCount.Add(900)
	dohFailureTotal.Add(2)

	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	before, _ := metricsCountersSnapshot()

	// Simulated restart: every atomic is gone, so only the file can carry the
	// totals forward.
	zeroCounters(t)
	resetMetricsPersist(t)
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, _ := metricsCountersSnapshot()

	if after.H1.FramesTx != before.H1.FramesTx || after.H1.FramesTx != 3 {
		t.Errorf("h1 frames tx = %d, want 3", after.H1.FramesTx)
	}
	if after.H1.BytesTx != 300 {
		t.Errorf("h1 bytes tx = %d, want 300", after.H1.BytesTx)
	}
	if after.H3.Attempts != 2 || after.H3.Connects != 1 {
		t.Errorf("h3 attempts/connects = %d/%d, want 2/1", after.H3.Attempts, after.H3.Connects)
	}
	if after.H3Datagram.Offered != 4 || after.H3Datagram.TxMessages != 9 || after.H3Datagram.TxBytes != 900 {
		t.Errorf("h3 datagram = %+v", after.H3Datagram)
	}
	if after.ContractsAcquired != 2 || after.ContractsDenied != 1 {
		t.Errorf("contracts = %d/%d, want 2/1", after.ContractsAcquired, after.ContractsDenied)
	}
	if after.DohFailures != 2 {
		t.Errorf("doh failures = %d, want 2", after.DohFailures)
	}
	if after.Errors[string(ErrorTransport)] != 2 || after.Errors[string(ErrorProxy)] != 1 {
		t.Errorf("errors = %v", after.Errors)
	}
}

// TestMetricsCountersNoNegativeStep is the acceptance property: a value read
// after a restart is never below the value read before it.
func TestMetricsCountersNoNegativeStep(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrError(ErrorIP)
	h1DirectStats.framesRx.Add(17)
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	before := scrapeValue(t, "urnet_errors_total{category=\"ip\"}")
	beforeFrames := scrapeValue(t, "urnet_transport_direct_h1_frames_total{dir=\"rx\"}")

	zeroCounters(t)
	resetMetricsPersist(t)
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := scrapeValue(t, "urnet_errors_total{category=\"ip\"}"); got < before {
		t.Errorf("errors_total fell across restart: %v -> %v", before, got)
	}
	if got := scrapeValue(t, "urnet_transport_direct_h1_frames_total{dir=\"rx\"}"); got < beforeFrames {
		t.Errorf("direct h1 frames fell across restart: %v -> %v", beforeFrames, got)
	}
}

// scrapeValue returns the numeric sample for one exact series line in the
// /metrics exposition.
func scrapeValue(t *testing.T, series string) float64 {
	t.Helper()
	_, body := scrape(t, "gzip")
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(rest, "%g", &v); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return v
	}
	t.Fatalf("series %q not found in exposition", series)
	return 0
}

func TestMetricsCountersRestoreRejectsBadFiles(t *testing.T) {
	saveCounters(t)

	cases := []struct {
		name string
		body string
	}{
		{"garbage", "{not json"},
		{"no version", `{"counters":{"contracts_acquired":5}}`},
		{"future version", `{"version":99,"counters":{"contracts_acquired":5}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zeroCounters(t)
			resetMetricsPersist(t)
			path := testStorePath(t)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := RestoreMetricsCounters(path); err != nil {
				t.Fatalf("restore must not fail on a bad file: %v", err)
			}
			if got := globalProm.contractsIn.Load(); got != 0 {
				t.Errorf("contracts = %d, want 0 (bad file must be ignored)", got)
			}
		})
	}
}

func TestMetricsCountersAtomicWriteKeepsPreviousFile(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A writer killed mid-write leaves a temp file behind and never touches
	// the live file, so the previous good snapshot must still parse and load.
	dir := filepath.Dir(path)
	stray := filepath.Join(dir, ".metrics-counters-dead.tmp")
	if err := os.WriteFile(stray, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	zeroCounters(t)
	resetMetricsPersist(t)
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore with a stray temp file: %v", err)
	}
	if got := globalProm.contractsIn.Load(); got != 1 {
		t.Fatalf("contracts = %d, want 1 from the intact file", got)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}

	// A completed flush leaves no temp file behind.
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".metrics-counters-") {
			t.Errorf("flush left a temp file behind: %s", entry.Name())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file metricsCountersFile
	if err := json.Unmarshal(after, &file); err != nil {
		t.Fatalf("rewritten store does not parse: %v", err)
	}
	if file.PID != os.Getpid() {
		t.Errorf("store pid = %d, want %d", file.PID, os.Getpid())
	}
	if len(good) == 0 {
		t.Fatal("first store was empty")
	}
}

// TestMetricsCountersFlushMergesHigherOnDiskValue covers the concurrent-writer
// guard: a process that loaded an older snapshot can never lower a series that
// another process already advanced.
func TestMetricsCountersFlushMergesHigherOnDiskValue(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	higher := metricsCountersFile{
		Version: metricsCountersVersion,
		SavedAt: time.Now().UTC(),
		Counters: metricsCounterTotals{
			ContractsAcquired: 500,
			H1:                modeCounters{FramesTx: 1000},
			Errors:            map[string]uint64{"transport": 7},
		},
		Proxies: map[string]metricsProxyCounters{
			"10.9.9.9:1080": {TotalRx: 9000, LastSeen: time.Now().Unix()},
		},
		Extras: map[string]map[string]uint64{"provider": {"control_commands.get": 12}},
	}
	blob, err := json.Marshal(higher)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// This process only knows a lower value; the flush must keep the higher one.
	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if file.Counters.ContractsAcquired < 501 {
		t.Errorf("contracts = %d, want >= 501", file.Counters.ContractsAcquired)
	}
	if file.Counters.H1.FramesTx < 1000 {
		t.Errorf("h1 frames = %d, want >= 1000", file.Counters.H1.FramesTx)
	}
	if file.Counters.Errors["transport"] < 7 {
		t.Errorf("transport errors = %d, want >= 7", file.Counters.Errors["transport"])
	}
	if got := file.Proxies["10.9.9.9:1080"].TotalRx; got < 9000 {
		t.Errorf("proxy rx = %d, want >= 9000", got)
	}
	if got := file.Extras["provider"]["control_commands.get"]; got < 12 {
		t.Errorf("extras counter = %d, want >= 12", got)
	}
}

func TestMetricsCountersProxyTotalsSurviveChurn(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)
	key := "10.42.42.42:1080"

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	bw := registerTestProxyWithTraffic(t, 910042, key, 1000, 40)
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Traffic after the last flush must survive the proxy leaving the pool.
	bw.TotalRx.Add(500)
	bw.BillableTx.Add(10)
	UnregisterProxy(910042)
	// The fold at unregister only reaches disk on the next flush; do it here so
	// the simulated restart sees everything the process knew.
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush after unregister: %v", err)
	}

	zeroCounters(t)
	resetMetricsPersist(t)
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	RegisterProxy(910043, key, key)
	reborn := RegisterProxyBandwidth(910043)
	t.Cleanup(func() { UnregisterProxy(910043) })

	if got := reborn.TotalRx.Load(); got != 1500 {
		t.Errorf("restored total rx = %d, want 1500 (1000 flushed + 500 folded at unregister)", got)
	}
	if got := reborn.BillableTx.Load(); got != 50 {
		t.Errorf("restored billable tx = %d, want 50", got)
	}

	// The /metrics exposition must carry the continued per-proxy series.
	_, body := scrape(t, "gzip")
	if !strings.Contains(body, `urnet_proxy_bytes_total{proxy="proxy[910043] (10.42.42.42:1080)",direction="in"} 1500`) {
		t.Errorf("per-proxy series not continued in exposition:\n%s", grepSeries(body, "urnet_proxy_bytes_total"))
	}
}

func TestMetricsCountersProxySeededAfterLateKey(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)
	key := "10.55.55.55:1080"

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	registerTestProxyWithTraffic(t, 910055, key, 700, 0)
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	zeroCounters(t)
	resetMetricsPersist(t)
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The bandwidth record exists before the identity is known, which is the
	// order RegisterProxyBandwidth/RegisterProxy can arrive in for the direct
	// identity. Seeding must still happen once the key is registered.
	bw := RegisterProxyBandwidth(910056)
	t.Cleanup(func() { UnregisterProxy(910056) })
	if got := bw.TotalRx.Load(); got != 0 {
		t.Fatalf("seeded before the key was known: rx = %d, want 0", got)
	}
	RegisterProxy(910056, key, key)
	if got := bw.TotalRx.Load(); got != 700 {
		t.Errorf("rx = %d after the key was registered, want 700", got)
	}
}

// TestUnregisterAfterEmptyFlushDoesNotPanic covers the base map going empty:
// a flush with no proxies prunes the base to nil, and the next unregister folds
// into it. CI caught this as "assignment to entry in nil map".
func TestUnregisterAfterEmptyFlushDoesNotPanic(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)
	key := "10.88.88.88:1080"

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// No proxies registered: this flush leaves the in-memory base empty.
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("empty flush: %v", err)
	}

	RegisterProxy(910088, key, key)
	RegisterProxyBandwidth(910088).TotalRx.Add(5)
	UnregisterProxy(910088) // must not panic on a nil base map

	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush after unregister: %v", err)
	}
	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := file.Proxies[key].TotalRx; got != 5 {
		t.Errorf("folded total rx = %d, want 5", got)
	}
}

func TestMetricsCountersProxyPruneAndCap(t *testing.T) {
	now := time.Now()
	old := now.Add(-metricsProxyRetention - time.Hour).Unix()
	live := map[string]metricsProxyCounters{"live": {TotalRx: 1, LastSeen: now.Unix()}}
	all := map[string]metricsProxyCounters{
		"live":  {TotalRx: 1, LastSeen: now.Unix()},
		"stale": {TotalRx: 2, LastSeen: old},
		"fresh": {TotalRx: 3, LastSeen: now.Unix()},
	}
	kept := pruneProxyCounters(all, live, now)
	if _, ok := kept["stale"]; ok {
		t.Error("entry older than the retention window was kept")
	}
	if len(kept) != 2 {
		t.Errorf("kept %d entries, want 2", len(kept))
	}

	// The cap drops dead entries first, oldest last-seen first, and never
	// drops a live one.
	big := map[string]metricsProxyCounters{}
	for i := 0; i < metricsProxyMaxEntries+10; i++ {
		big["dead-"+strconv.Itoa(i)] = metricsProxyCounters{
			LastSeen: now.Add(-time.Duration(i) * time.Second).Unix(),
		}
	}
	big["live"] = metricsProxyCounters{LastSeen: now.Unix()}
	capped := pruneProxyCounters(big, live, now)
	if len(capped) != metricsProxyMaxEntries {
		t.Errorf("capped map has %d entries, want %d", len(capped), metricsProxyMaxEntries)
	}
	if _, ok := capped["live"]; !ok {
		t.Error("the live entry was dropped by the cap")
	}

	// A live set larger than the cap keeps every live proxy: the cap bounds
	// churn, it never deletes what the node is serving.
	wideLive := map[string]metricsProxyCounters{}
	wideAll := map[string]metricsProxyCounters{}
	for i := 0; i < metricsProxyMaxEntries+5; i++ {
		key := "live-" + strconv.Itoa(i)
		wideLive[key] = metricsProxyCounters{LastSeen: now.Unix()}
		wideAll[key] = metricsProxyCounters{LastSeen: now.Unix()}
	}
	wide := pruneProxyCounters(wideAll, wideLive, now)
	if len(wide) != len(wideLive) {
		t.Errorf("pruned a live set of %d down to %d", len(wideLive), len(wide))
	}
}

func TestMetricsCountersPersistentSourceRoundTrip(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	calls := map[string]uint64{}
	RegisterPersistentCounterSource(PersistentCounterSource{
		Name:     "test",
		Snapshot: func() map[string]uint64 { return map[string]uint64{"thing": 4} },
		Restore:  func(vals map[string]uint64) { calls["restored"] = vals["thing"] },
	})
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := file.Extras["test"]["thing"]; got != 4 {
		t.Fatalf("stored extras = %d, want 4", got)
	}

	// A fresh process: the source is registered before the restore runs.
	resetMetricsPersist(t)
	RegisterPersistentCounterSource(PersistentCounterSource{
		Name:     "test",
		Snapshot: func() map[string]uint64 { return map[string]uint64{"thing": 4} },
		Restore:  func(vals map[string]uint64) { calls["restored"] = vals["thing"] },
	})
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if calls["restored"] != 4 {
		t.Errorf("source restore got %d, want 4", calls["restored"])
	}
}

// TestMetricsCountersPendingSourceRestore covers a source that registers AFTER
// the store was restored (the provider registers early, but the contract is
// order-independent).
func TestMetricsCountersPendingSourceRestore(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	file := metricsCountersFile{
		Version: metricsCountersVersion,
		SavedAt: time.Now().UTC(),
		Extras:  map[string]map[string]uint64{"late": {"x": 3}},
	}
	blob, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var got uint64
	RegisterPersistentCounterSource(PersistentCounterSource{
		Name:     "late",
		Snapshot: func() map[string]uint64 { return map[string]uint64{"x": got} },
		Restore:  func(vals map[string]uint64) { got = vals["x"] },
	})
	if got != 3 {
		t.Errorf("late source got %d, want 3", got)
	}
}

func TestProcessStartTimeGaugeExported(t *testing.T) {
	_, body := scrape(t, "")
	want := globalProm.startTime.Unix()
	if !strings.Contains(body, "urnet_process_start_time_seconds ") {
		t.Fatal("urnet_process_start_time_seconds missing from exposition")
	}
	if got := scrapeValue(t, "urnet_process_start_time_seconds"); int64(got) != want {
		t.Errorf("process start = %d, want %d", int64(got), want)
	}
	if !strings.Contains(body, "# TYPE urnet_process_start_time_seconds gauge") {
		t.Error("process start time must be a gauge")
	}
}

func TestDohFailureTotalIsCumulative(t *testing.T) {
	saveCounters(t)
	zeroCounters(t)
	dohFailureCount.Add(3)
	dohFailureTotal.Add(3)
	if got := GetDohFailureTotal(); got != 3 {
		t.Fatalf("first total read = %d, want 3", got)
	}
	if got := GetDohFailureTotal(); got != 3 {
		t.Fatalf("second total read = %d, want 3 (a counter must not reset on read)", got)
	}
	// The interval counter the health heartbeat consumes keeps its delta
	// contract: first read takes the 3, the next read is 0.
	if got := GetDohFailureCount(); got != 3 {
		t.Fatalf("interval read = %d, want 3", got)
	}
	if got := GetDohFailureCount(); got != 0 {
		t.Fatalf("interval read after drain = %d, want 0", got)
	}
	if got := GetDohFailureTotal(); got != 3 {
		t.Fatalf("cumulative read after an interval drain = %d, want 3", got)
	}
}

// TestMetricsCountersFlushRefusesNewerSchema: a store written by a newer build
// must never be overwritten in this build's schema, or its unknown fields are
// lost on a downgrade.
func TestMetricsCountersFlushRefusesNewerSchema(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	newer := []byte(`{"version":99,"future_field":{"x":1},"counters":{"contracts_acquired":7},"saved_at":"2026-10-20T00:00:00Z"}`)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore must not fail on a newer store: %v", err)
	}
	IncrContractAcquired()
	err := FlushMetricsCounters()
	if !errors.Is(err, errMetricsCountersNewer) {
		t.Fatalf("flush error = %v, want errMetricsCountersNewer", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(newer) {
		t.Errorf("the newer store was modified:\n%s", after)
	}
}

// TestAddInt64CounterSaturates: a stored total near MaxInt64 must clamp, not
// wrap the counter negative (which the scrape would read as zero).
func TestAddInt64CounterSaturates(t *testing.T) {
	var c atomic.Int64
	c.Store(math.MaxInt64 - 5)
	addInt64Counter(&c, 10)
	if got := c.Load(); got != math.MaxInt64 {
		t.Fatalf("saturated value = %d, want %d", got, int64(math.MaxInt64))
	}
	var d atomic.Int64
	d.Store(3)
	addInt64Counter(&d, math.MaxInt64)
	if got := d.Load(); got != math.MaxInt64 {
		t.Fatalf("saturated value = %d, want %d", got, int64(math.MaxInt64))
	}
}

// TestCatchUpMetricsCounters covers a promoted HotSwap child: its own counters
// sit below what the parent wrote after the child restored, and catch-up raises
// it to the stored totals without lowering anything.
func TestCatchUpMetricsCounters(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)
	key := "10.66.66.66:1080"

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// This process has counted a little; the store has counted more.
	IncrContractAcquired()
	h1ModeStats.framesTx.Add(4)
	file := metricsCountersFile{
		Version: metricsCountersVersion,
		SavedAt: time.Now().UTC(),
		Counters: metricsCounterTotals{
			ContractsAcquired: 50,
			H1:                modeCounters{FramesTx: 900, BytesTx: 9000},
		},
		Proxies: map[string]metricsProxyCounters{
			key:                {TotalRx: 1234, BillableTx: 12, LastSeen: time.Now().Unix()},
			"10.77.77.77:1080": {TotalRx: 500, LastSeen: time.Now().Unix()},
		},
		Extras: map[string]map[string]uint64{"raise_test": {"thing": 9}},
	}
	blob, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	bw := registerTestProxyWithTraffic(t, 910066, key, 10, 1)
	var raised map[string]uint64
	RegisterPersistentCounterSource(PersistentCounterSource{
		Name:     "raise_test",
		Snapshot: func() map[string]uint64 { return map[string]uint64{"thing": 1} },
		Raise:    func(vals map[string]uint64) { raised = vals },
	})
	if err := CatchUpMetricsCounters(); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if got := globalProm.contractsIn.Load(); got != 50 {
		t.Errorf("contracts = %d, want 50", got)
	}
	if got := h1ModeStats.framesTx.Load(); got != 900 {
		t.Errorf("h1 frames = %d, want 900", got)
	}
	if got := h1ModeStats.bytesTx.Load(); got != 9000 {
		t.Errorf("h1 bytes = %d, want 9000", got)
	}
	if got := bw.TotalRx.Load(); got != 1234 {
		t.Errorf("proxy rx = %d, want 1234", got)
	}
	if got := bw.BillableTx.Load(); got != 12 {
		t.Errorf("proxy billable tx = %d, want 12", got)
	}
	if raised["thing"] != 9 {
		t.Errorf("source raise got %v, want thing=9", raised)
	}
	// A stored identity that has not registered yet must still reach the base:
	// the proxy seeds from it when the staggered launch gets there.
	RegisterProxy(910077, "10.77.77.77:1080", "10.77.77.77:1080")
	late := RegisterProxyBandwidth(910077)
	t.Cleanup(func() { UnregisterProxy(910077) })
	if got := late.TotalRx.Load(); got != 500 {
		t.Errorf("late-registered proxy rx = %d, want 500 from the store", got)
	}

	// Catch-up never lowers: a second call with the same store changes nothing.
	if err := CatchUpMetricsCounters(); err != nil {
		t.Fatalf("second catch up: %v", err)
	}
	if got := bw.TotalRx.Load(); got != 1234 {
		t.Errorf("proxy rx after a second catch-up = %d, want 1234", got)
	}
}

func grepSeries(body, prefix string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// The re-exec phases below exercise the real restart path: two separate OS
// processes, the first writing the store before it exits and the second
// loading it. That covers what an in-process simulated restart cannot — a
// genuinely empty address space and a fresh package init.
const (
	metricsPersistPhaseEnv = "METRICS_PERSIST_PHASE"
	metricsPersistPathEnv  = "METRICS_PERSIST_PATH"
	metricsProbePrefix     = "PROBE "
)

func TestMetricsCountersSurviveProcessRestart(t *testing.T) {
	switch os.Getenv(metricsPersistPhaseEnv) {
	case "write":
		metricsPersistChildWrite()
		return
	case "read":
		metricsPersistChildRead()
		return
	}

	path := filepath.Join(t.TempDir(), "metrics_counters.json")
	runMetricsPersistChild(t, path, "write")
	out := runMetricsPersistChild(t, path, "read")

	for _, want := range []string{
		`urnet_errors_total{category="transport"} 2`,
		`urnet_contracts_total{result="acquired"} 1`,
		`urnet_proxy_bytes_total{proxy="proxy[1] (10.7.7.7:1080)",direction="in"} 4096`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second process did not continue %q\n%s", want, out)
		}
	}
}

func metricsPersistChildWrite() {
	path := os.Getenv(metricsPersistPathEnv)
	if err := RestoreMetricsCounters(path); err != nil {
		fmt.Printf("%schild restore: %v\n", metricsProbePrefix, err)
		os.Exit(1)
	}
	IncrError(ErrorTransport)
	IncrError(ErrorTransport)
	IncrContractAcquired()
	RegisterProxy(1, "10.7.7.7:1080", "10.7.7.7:1080")
	RegisterProxyBandwidth(1).TotalRx.Add(4096)
	if err := FlushMetricsCounters(); err != nil {
		fmt.Printf("%sflush: %v\n", metricsProbePrefix, err)
		os.Exit(1)
	}
	fmt.Printf("%swritten\n", metricsProbePrefix)
}

func metricsPersistChildRead() {
	path := os.Getenv(metricsPersistPathEnv)
	if err := RestoreMetricsCounters(path); err != nil {
		fmt.Printf("%schild restore: %v\n", metricsProbePrefix, err)
		os.Exit(1)
	}
	RegisterProxy(1, "10.7.7.7:1080", "10.7.7.7:1080")
	RegisterProxyBandwidth(1)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(rec, req)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "urnet_errors_total") ||
			strings.HasPrefix(line, "urnet_contracts_total") ||
			strings.HasPrefix(line, "urnet_proxy_bytes_total") {
			fmt.Printf("%s%s\n", metricsProbePrefix, line)
		}
	}
}

func runMetricsPersistChild(t *testing.T, path, phase string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMetricsCountersSurviveProcessRestart$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		metricsPersistPhaseEnv+"="+phase,
		metricsPersistPathEnv+"="+path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process (%s): %v\n%s", phase, err, out)
	}
	return string(out)
}

// backdateStore rewrites the store with mutate applied and an old mtime, so a
// later flush that leaves the file alone is distinguishable from one that
// rewrites it (a rewrite renames a fresh file over it).
func backdateStore(t *testing.T, path string, mutate func(*metricsCountersFile)) time.Time {
	t.Helper()
	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if mutate != nil {
		mutate(&file)
	}
	blob, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return old
}

func storeModTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	return info.ModTime()
}

// An idle node must not rewrite the store: every write is an fsync plus a
// directory fsync, which is what wears an SD card.
func TestMetricsCountersFlushSkipsUnchangedStore(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrContractAcquired()
	registerTestProxyWithTraffic(t, 41, "198.51.100.41:1080", 100, 50)
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The live proxy's last_seen is restamped on every flush; a stale value on
	// disk must not make an otherwise identical store look changed.
	old := backdateStore(t, path, func(f *metricsCountersFile) {
		for k, v := range f.Proxies {
			v.LastSeen = 1
			f.Proxies[k] = v
		}
	})
	for i := 0; i < 3; i++ {
		if err := FlushMetricsCounters(); err != nil {
			t.Fatalf("idle flush %d: %v", i, err)
		}
	}
	if got := storeModTime(t, path); !got.Equal(old) {
		t.Fatalf("an idle flush rewrote the store (mtime %v, want the untouched %v)", got, old)
	}
}

func TestMetricsCountersFlushRewritesWhenCountersMove(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	old := backdateStore(t, path, nil)

	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := storeModTime(t, path); !got.After(old) {
		t.Fatalf("a changed counter was not written (mtime %v, backdated %v)", got, old)
	}
	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if file.Counters.ContractsAcquired != 2 {
		t.Errorf("contracts acquired = %d, want 2", file.Counters.ContractsAcquired)
	}
}

// An unchanged store is still refreshed once it is a day old, so saved_at and
// the proxy retention stamps do not go stale on a node that is idle for weeks.
func TestMetricsCountersFlushRefreshesAStaleStore(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	old := backdateStore(t, path, func(f *metricsCountersFile) {
		f.SavedAt = time.Now().Add(-(metricsUnchangedRewriteAge + time.Hour)).UTC()
	})

	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := storeModTime(t, path); !got.After(old) {
		t.Fatalf("a day-old unchanged store was not refreshed (mtime %v)", got)
	}
	file, err := readMetricsCountersFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if time.Since(file.SavedAt) > time.Minute {
		t.Errorf("saved_at = %v, want a fresh stamp", file.SavedAt)
	}
}

// A deleted or unreadable store is not "unchanged": it is recreated.
func TestMetricsCountersFlushRecreatesAMissingStore(t *testing.T) {
	saveCounters(t)
	resetMetricsPersist(t)
	path := testStorePath(t)

	if err := RestoreMetricsCounters(path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	IncrContractAcquired()
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := FlushMetricsCounters(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store was not recreated: %v", err)
	}
}
