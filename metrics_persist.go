package connect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Cumulative /metrics counters are process-lifetime by construction: they are
// plain atomics seeded at zero. A restart of the provider (rolling upgrade,
// reboot, crash, hotswap) therefore reset every urnet_*_total series to zero,
// which loses the per-node traffic history a restart-surviving 24h
// attribution needs. This file gives them back: the totals are written to a
// versioned JSON store under ~/.urnetwork, and at startup the stored value is
// ADDED into each atomic before the /metrics listener serves, so every series
// continues from where the previous process stopped instead of stepping back
// to zero.
//
// The store is deliberately separate from lifetime_metrics.json. That file is
// the all-time accounting ledger (sessions, contract transitions, billable
// bytes) with its own delta-based collectors; mixing the raw counters into it
// would double count what those collectors already add up.
//
// What is NOT persisted, and why: gauges (they describe the current process),
// Go runtime counters (GC cycles belong to this process), and the per-proxy
// client/session gauges. Only monotonic totals are stored.

const (
	// metricsCountersVersion is bumped when the on-disk shape changes in a way
	// an older reader would misinterpret. A file written by a NEWER version is
	// ignored rather than guessed at; a file written by an older one is read,
	// because every field is additive and absent fields simply stay zero.
	metricsCountersVersion = 1

	// DefaultMetricsCountersFlushInterval is how often the store is rewritten.
	// The interval bounds what a SIGKILL can lose; 45s keeps that small without
	// turning a quiet node's state file into a write-rate item.
	DefaultMetricsCountersFlushInterval = 45 * time.Second

	// metricsProxyRetention drops per-proxy entries that have not been live or
	// flushed for this long. A proxy absent for a month is not coming back with
	// the same identity in any way that its old byte count should follow.
	metricsProxyRetention = 30 * 24 * time.Hour

	// metricsProxyMaxEntries caps the per-proxy map even inside the retention
	// window. A node churning URL-sourced proxies can see thousands of
	// addresses; the oldest non-live entries are dropped first, so a live proxy
	// is never pruned by the cap.
	metricsProxyMaxEntries = 2048

	// metricsUnchangedRewriteAge is how stale the store may grow while nothing
	// changes. An idle node skips the write entirely (every write is an fsync
	// and a directory fsync, which is what wears an SD card), but the proxy
	// retention clock reads LastSeen and the file's saved_at is what an
	// operator checks for liveness, so an unchanged store is still refreshed
	// this often.
	metricsUnchangedRewriteAge = 24 * time.Hour
)

// modeCounters is one transport mode's cumulative totals. The lifecycle fields
// stay zero for H1, which has no connect-attempt accounting of its own.
type modeCounters struct {
	FramesTx        uint64 `json:"frames_tx"`
	FramesRx        uint64 `json:"frames_rx"`
	BytesTx         uint64 `json:"bytes_tx"`
	BytesRx         uint64 `json:"bytes_rx"`
	Attempts        uint64 `json:"attempts,omitempty"`
	Connects        uint64 `json:"connects,omitempty"`
	ConnectFailures uint64 `json:"connect_failures,omitempty"`
	Drops           uint64 `json:"drops,omitempty"`
}

// datagramCounters is the H3 DATAGRAM block.
type datagramCounters struct {
	Offered          uint64 `json:"offered"`
	Accepted         uint64 `json:"accepted"`
	RxDrops          uint64 `json:"rx_drops"`
	RxMessages       uint64 `json:"rx_messages"`
	RxBytes          uint64 `json:"rx_bytes"`
	RxMalformed      uint64 `json:"rx_malformed"`
	RxDuplicate      uint64 `json:"rx_duplicate"`
	RxChecksum       uint64 `json:"rx_checksum"`
	TxMessages       uint64 `json:"tx_messages"`
	TxBytes          uint64 `json:"tx_bytes"`
	TxStreamMessages uint64 `json:"tx_stream_messages"`
	TxErrors         uint64 `json:"tx_errors"`
	Blackholes       uint64 `json:"blackholes"`
}

type metricsCounterTotals struct {
	H1         modeCounters     `json:"h1"`
	H1Direct   modeCounters     `json:"h1_direct"`
	H3         modeCounters     `json:"h3"`
	H3Dns      modeCounters     `json:"h3_dns"`
	H3DnsPump  modeCounters     `json:"h3_dns_pump"`
	H3Datagram datagramCounters `json:"h3_datagram"`

	Errors            map[string]uint64 `json:"errors,omitempty"`
	ContractsAcquired uint64            `json:"contracts_acquired"`
	ContractsDenied   uint64            `json:"contracts_denied"`
	DohFailures       uint64            `json:"doh_failures"`
}

// metricsProxyCounters is one proxy identity's cumulative byte totals.
type metricsProxyCounters struct {
	TotalRx    uint64 `json:"total_rx"`
	TotalTx    uint64 `json:"total_tx"`
	BillableRx uint64 `json:"billable_rx"`
	BillableTx uint64 `json:"billable_tx"`
	LastSeen   int64  `json:"last_seen_unix"`
}

// metricsCountersFile is the on-disk shape. PID and ProcessStartUnix are
// written for operators and for diagnosing a concurrent-writer merge; they
// never decide whether a value is valid.
type metricsCountersFile struct {
	Version          int                             `json:"version"`
	SavedAt          time.Time                       `json:"saved_at"`
	PID              int                             `json:"pid"`
	ProcessStartUnix int64                           `json:"process_start_unix"`
	Counters         metricsCounterTotals            `json:"counters"`
	Proxies          map[string]metricsProxyCounters `json:"proxies,omitempty"`
	Extras           map[string]map[string]uint64    `json:"extras,omitempty"`
}

// PersistentCounterSource lets a package that is not connect (the provider)
// include its own cumulative counters in the same store. Snapshot returns the
// current totals; Restore is called once with the values found on disk, which
// the source adds into its own atomics. Restore may arrive before or after
// registration: values for an unregistered source are held until it appears.
type PersistentCounterSource struct {
	Name     string
	Snapshot func() map[string]uint64
	Restore  func(map[string]uint64)
	// Raise is the catch-up counterpart of Restore: it takes the source's
	// counters to at least the stored values without adding them a second time.
	// Optional; a source without it is only restored at startup.
	Raise func(map[string]uint64)
}

var (
	metricsPersistMu sync.Mutex
	// metricsFlushMu serialises flushes inside this process, so the periodic
	// writer and a shutdown (or catch-up) flush cannot interleave their
	// read-merge-write sequences.
	metricsFlushMu sync.Mutex
	// metricsPersistPath is the store path in use; "" until a restore or start.
	metricsPersistPath string
	// metricsProxyBase holds the pre-process totals per proxy identity. It is
	// the seed for a freshly registered ProxyBandwidth and the value kept for
	// an identity that is no longer live.
	metricsProxyBase = map[string]metricsProxyCounters{}
	// metricsRestored makes Restore idempotent: the totals are added into the
	// process atomics, so a second call would double them.
	metricsRestored bool

	metricsFlushStop chan struct{}
	metricsFlushDone chan struct{}

	persistentSourcesMu sync.Mutex
	persistentSources   = map[string]PersistentCounterSource{}
	// pendingSourceTotals holds counters loaded before their source registered.
	pendingSourceTotals = map[string]map[string]uint64{}
	// pendingSourceRaises is the same for a catch-up that arrived before the
	// source registered: applied with Raise, never with Restore, so a late
	// registration cannot double count.
	pendingSourceRaises = map[string]map[string]uint64{}
)

// RestoreMetricsCounters reads path and adds the stored totals into the
// process counters. A missing, unreadable, corrupt, or newer-versioned file is
// not an error: the provider starts from zero exactly as it did before this
// feature existed, and the reason is reported on stderr. It must be called
// before the /metrics listener serves so the first scrape already sees the
// continued series.
func RestoreMetricsCounters(path string) error {
	if path == "" {
		return fmt.Errorf("metrics counters: empty path")
	}
	metricsPersistMu.Lock()
	if metricsRestored {
		metricsPersistMu.Unlock()
		return fmt.Errorf("metrics counters: already restored")
	}
	metricsRestored = true
	metricsPersistPath = path
	metricsPersistMu.Unlock()

	file, err := readMetricsCountersFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "[metrics] counters not restored from %s: %v (starting from zero)\n", path, err)
		}
		return nil
	}

	seedMetricsCounters(file.Counters)
	seedProxyBases(file.Proxies)
	restoreSourceTotals(file.Extras)
	return nil
}

// CatchUpMetricsCounters raises this process's counters to whatever another
// process has already written to the store, and seeds any proxy identity the
// store knows about whose record this process created without a base.
//
// A promoted HotSwap child restores at spawn time, before the parent's final
// flush, so its in-memory totals can sit below what the parent wrote during
// the drain. Calling this at takeover closes that gap: the child continues
// from the parent's last write instead of from its own older snapshot. Every
// value is raised, never lowered, so catching up twice is harmless.
func CatchUpMetricsCounters() error {
	metricsPersistMu.Lock()
	path := metricsPersistPath
	metricsPersistMu.Unlock()
	if path == "" {
		return nil
	}
	file, err := readMetricsCountersFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	catchUpCounterTotals(file.Counters)
	// Refresh the pre-process base as well: a proxy that has not registered yet
	// in this process seeds from this map when it does, and the parent may have
	// advanced its totals after this process restored at spawn time.
	seedProxyBases(file.Proxies)
	for key, want := range file.Proxies {
		catchUpProxyBandwidth(key, want)
	}
	raiseSourceTotals(file.Extras)
	return nil
}

// catchUpCounterTotals raises each counter to the stored value when the stored
// value is ahead. The reads and adds are atomic, so a concurrent increment
// only makes the final value slightly larger, never smaller.
func catchUpCounterTotals(c metricsCounterTotals) {
	raiseModeCounters(&h1ModeStats, c.H1)
	raiseModeCounters(&h1DirectStats, c.H1Direct)
	raiseH3Family(&h3Counters, c.H3)
	raiseH3Family(&h3DnsCounters, c.H3Dns)
	raiseH3Family(&h3DnsPumpCounters, c.H3DnsPump)
	raiseDatagramCounters(c.H3Datagram)

	globalProm.initErrors()
	for cat, v := range c.Errors {
		if dst, ok := globalProm.errorsTotal[ErrorCategory(cat)]; ok {
			raiseI64(dst, v)
		}
	}
	raiseI64(&globalProm.contractsIn, c.ContractsAcquired)
	raiseI64(&globalProm.contractsOut, c.ContractsDenied)
	raiseI64(&dohFailureTotal, c.DohFailures)
}

func raiseModeCounters(s *transportModeStats, c modeCounters) {
	raiseU64(&s.framesTx, c.FramesTx)
	raiseU64(&s.framesRx, c.FramesRx)
	raiseU64(&s.bytesTx, c.BytesTx)
	raiseU64(&s.bytesRx, c.BytesRx)
}

func raiseH3Family(c *h3FamilyCounters, m modeCounters) {
	raiseModeCounters(&c.transportModeStats, m)
	raiseU64(&c.attempts, m.Attempts)
	raiseU64(&c.connects, m.Connects)
	raiseU64(&c.connectFailures, m.ConnectFailures)
	raiseU64(&c.drops, m.Drops)
}

func raiseDatagramCounters(c datagramCounters) {
	raiseU64(&h3DatagramOffered, c.Offered)
	raiseU64(&h3DatagramAccepted, c.Accepted)
	raiseU64(&h3DatagramDrops, c.RxDrops)
	raiseU64(&h3DatagramStats.receivedMessageCount, c.RxMessages)
	raiseU64(&h3DatagramStats.receivedMessageByteCount, c.RxBytes)
	raiseU64(&h3DatagramStats.malformedFragmentCount, c.RxMalformed)
	raiseU64(&h3DatagramStats.duplicateFragmentCount, c.RxDuplicate)
	raiseU64(&h3DatagramStats.checksumFailureCount, c.RxChecksum)
	raiseU64(&h3DatagramStats.sentMessageCount, c.TxMessages)
	raiseU64(&h3DatagramStats.sentMessageByteCount, c.TxBytes)
	raiseU64(&h3DatagramStats.streamSentMessageCount, c.TxStreamMessages)
	raiseU64(&h3DatagramStats.sendErrorCount, c.TxErrors)
	raiseU64(&h3DatagramBlackholes, c.Blackholes)
}

// StartMetricsCounters starts the periodic flush. interval <= 0 selects
// DefaultMetricsCountersFlushInterval. Calling it twice is a no-op, so a
// process that restores and then takes over a hotswap cannot start two writers.
func StartMetricsCounters(path string, interval time.Duration) {
	if path == "" {
		return
	}
	if interval <= 0 {
		interval = DefaultMetricsCountersFlushInterval
	}
	metricsPersistMu.Lock()
	if metricsFlushStop != nil {
		metricsPersistMu.Unlock()
		return
	}
	metricsPersistPath = path
	stop := make(chan struct{})
	done := make(chan struct{})
	metricsFlushStop, metricsFlushDone = stop, done
	metricsPersistMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastErr string
		for {
			select {
			case <-ticker.C:
				err := FlushMetricsCounters()
				if err == nil {
					lastErr = ""
					continue
				}
				// A state directory that cannot be written fails every tick.
				// Report the first failure and then only a CHANGE, so a
				// read-only home does not fill the log with one line per
				// interval.
				if msg := err.Error(); msg != lastErr {
					fmt.Fprintf(os.Stderr, "[metrics] counter flush failed: %v\n", err)
					lastErr = msg
				}
			case <-stop:
				return
			}
		}
	}()
}

// StopMetricsCounters stops the periodic flush and waits for the writer to
// exit. It does not write; the shutdown path calls FlushMetricsCounters after
// this so the final values are on disk before the process exits.
func StopMetricsCounters() {
	metricsPersistMu.Lock()
	stop, done := metricsFlushStop, metricsFlushDone
	metricsFlushStop, metricsFlushDone = nil, nil
	metricsPersistMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// FlushMetricsCounters writes the current totals to the store atomically. It
// is safe to call from the periodic writer and from the shutdown path.
//
// The write merges with whatever is already on disk using max() per series,
// and the in-memory pre-process base keeps the higher of the two as well. That
// is the multi-process guard: during a hotswap the draining parent and the
// promoted child both flush to the same path, and a plain overwrite would let
// the process that loaded the older snapshot push the file backwards. With the
// merge no writer can lower a series, so the file only moves forward.
func FlushMetricsCounters() error {
	metricsFlushMu.Lock()
	defer metricsFlushMu.Unlock()

	metricsPersistMu.Lock()
	path := metricsPersistPath
	base := make(map[string]metricsProxyCounters, len(metricsProxyBase))
	for k, v := range metricsProxyBase {
		base[k] = v
	}
	metricsPersistMu.Unlock()
	if path == "" {
		return nil
	}

	now := time.Now()
	totals, live := metricsCountersSnapshot()
	extras := snapshotPersistentSources()

	prior, priorErr := readMetricsCountersFile(path)
	if priorErr != nil && errors.Is(priorErr, errMetricsCountersNewer) {
		// A newer build wrote this store. Its fields are unknown to us, so
		// rewriting it in this build's schema would destroy them; leave it
		// alone and keep running from zero rather than corrupt the newer state.
		return priorErr
	}
	if priorErr == nil {
		totals = mergeCounterTotals(totals, prior.Counters)
		base = mergeProxyMaps(base, prior.Proxies)
		extras = mergeExtraSources(extras, prior.Extras)
	}
	base = mergeProxyMaps(base, live)
	for k := range live {
		entry := base[k]
		entry.LastSeen = now.Unix()
		base[k] = entry
	}
	proxies := pruneProxyCounters(base, live, now)

	// Keep the raised base in memory too, pruned the same way: a proxy that
	// unregisters right after this flush must not seed its replacement below
	// the total we just wrote, and the base must not grow for the life of a
	// churning node. A fold that lands after the lock is taken applies to the
	// replaced map, and one that landed before is already in it, so no folded
	// total is lost either way.
	metricsPersistMu.Lock()
	pruned := pruneProxyCounters(mergeProxyMaps(metricsProxyBase, proxies), live, now)
	if pruned == nil {
		// Keep the base writable: a fold from UnregisterProxy writes into it,
		// and prune returns nil for an empty set.
		pruned = map[string]metricsProxyCounters{}
	}
	metricsProxyBase = pruned
	metricsPersistMu.Unlock()

	file := metricsCountersFile{
		Version:          metricsCountersVersion,
		SavedAt:          now.UTC(),
		PID:              os.Getpid(),
		ProcessStartUnix: globalProm.startTime.Unix(),
		Counters:         totals,
		Proxies:          proxies,
		Extras:           extras,
	}
	if priorErr == nil && countersFileUnchanged(file, prior, now) {
		return nil
	}
	blob, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeFileAtomicFsync(path, append(blob, '\n'), 0o600)
}

// countersFileUnchanged reports whether writing next would leave the store
// saying the same thing prior already does. saved_at, the writer's pid and
// every proxy's last_seen are stamped afresh on each flush, so they are left
// out of the comparison; a store older than metricsUnchangedRewriteAge is
// rewritten anyway so those stamps do not go stale on an idle node.
func countersFileUnchanged(next, prior metricsCountersFile, now time.Time) bool {
	if prior.SavedAt.IsZero() || now.Sub(prior.SavedAt) >= metricsUnchangedRewriteAge {
		return false
	}
	next.SavedAt, prior.SavedAt = time.Time{}, time.Time{}
	next.PID, prior.PID = 0, 0
	next.Proxies = withoutLastSeen(next.Proxies)
	prior.Proxies = withoutLastSeen(prior.Proxies)
	a, errA := json.Marshal(next)
	b, errB := json.Marshal(prior)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func withoutLastSeen(in map[string]metricsProxyCounters) map[string]metricsProxyCounters {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]metricsProxyCounters, len(in))
	for k, v := range in {
		v.LastSeen = 0
		out[k] = v
	}
	return out
}

// metricsCountersSnapshot returns the current totals and the live per-proxy
// totals keyed by proxy identity.
func metricsCountersSnapshot() (metricsCounterTotals, map[string]metricsProxyCounters) {
	totals := snapshotCounterTotals()
	proxies := map[string]metricsProxyCounters{}
	for key, bw := range ProxyBandwidthSnapshotByKey() {
		if bw == nil {
			continue
		}
		proxies[key] = metricsProxyCounters{
			TotalRx:    bw.TotalRx.Load(),
			TotalTx:    bw.TotalTx.Load(),
			BillableRx: bw.BillableRx.Load(),
			BillableTx: bw.BillableTx.Load(),
			LastSeen:   time.Now().Unix(),
		}
	}
	return totals, proxies
}

// RegisterPersistentCounterSource adds a non-connect package's counters to the
// store. If a store was already restored, its values for this name are applied
// immediately; otherwise they are applied when RestoreMetricsCounters runs.
func RegisterPersistentCounterSource(src PersistentCounterSource) {
	if src.Name == "" || src.Snapshot == nil {
		return
	}
	persistentSourcesMu.Lock()
	persistentSources[src.Name] = src
	pending, hasPending := pendingSourceTotals[src.Name]
	if hasPending {
		delete(pendingSourceTotals, src.Name)
	}
	pendingRaise, hasPendingRaise := pendingSourceRaises[src.Name]
	if hasPendingRaise {
		delete(pendingSourceRaises, src.Name)
	}
	persistentSourcesMu.Unlock()

	if hasPending && src.Restore != nil {
		src.Restore(pending)
	}
	if hasPendingRaise && src.Raise != nil {
		src.Raise(pendingRaise)
	}
}

func snapshotPersistentSources() map[string]map[string]uint64 {
	persistentSourcesMu.Lock()
	sources := make([]PersistentCounterSource, 0, len(persistentSources))
	for _, src := range persistentSources {
		sources = append(sources, src)
	}
	persistentSourcesMu.Unlock()

	if len(sources) == 0 {
		return nil
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	out := make(map[string]map[string]uint64, len(sources))
	for _, src := range sources {
		vals := src.Snapshot()
		if len(vals) == 0 {
			continue
		}
		out[src.Name] = mergeExtraCounters(out[src.Name], vals)
	}
	return out
}

func restoreSourceTotals(extras map[string]map[string]uint64) {
	if len(extras) == 0 {
		return
	}
	for name, vals := range extras {
		persistentSourcesMu.Lock()
		src, ok := persistentSources[name]
		if !ok {
			pendingSourceTotals[name] = vals
		}
		persistentSourcesMu.Unlock()
		if ok && src.Restore != nil {
			src.Restore(vals)
		}
	}
}

// raiseSourceTotals is restoreSourceTotals for a catch-up: each source is
// raised to at least the stored values (never added), which is what makes
// catching up twice harmless.
func raiseSourceTotals(extras map[string]map[string]uint64) {
	if len(extras) == 0 {
		return
	}
	for name, vals := range extras {
		persistentSourcesMu.Lock()
		src, ok := persistentSources[name]
		if !ok {
			pendingSourceRaises[name] = mergeExtraCounters(pendingSourceRaises[name], vals)
		}
		persistentSourcesMu.Unlock()
		if ok && src.Raise != nil {
			src.Raise(vals)
		}
	}
}

// --- seeding the connect-owned counters ---

func seedMetricsCounters(c metricsCounterTotals) {
	seedModeCounters(&h1ModeStats, c.H1)
	seedModeCounters(&h1DirectStats, c.H1Direct)
	seedH3Family(&h3Counters, c.H3)
	seedH3Family(&h3DnsCounters, c.H3Dns)
	seedH3Family(&h3DnsPumpCounters, c.H3DnsPump)
	seedDatagramCounters(c.H3Datagram)

	globalProm.initErrors()
	for cat, v := range c.Errors {
		if dst, ok := globalProm.errorsTotal[ErrorCategory(cat)]; ok {
			addInt64Counter(dst, v)
		}
	}
	addInt64Counter(&globalProm.contractsIn, c.ContractsAcquired)
	addInt64Counter(&globalProm.contractsOut, c.ContractsDenied)
	addInt64Counter(&dohFailureTotal, c.DohFailures)
}

func snapshotCounterTotals() metricsCounterTotals {
	globalProm.initErrors()
	totals := metricsCounterTotals{
		H1:         snapshotModeCounters(&h1ModeStats),
		H1Direct:   snapshotModeCounters(&h1DirectStats),
		H3:         snapshotH3Family(&h3Counters),
		H3Dns:      snapshotH3Family(&h3DnsCounters),
		H3DnsPump:  snapshotH3Family(&h3DnsPumpCounters),
		H3Datagram: snapshotDatagramCounters(),
		Errors:     map[string]uint64{},
	}
	for cat, c := range globalProm.errorsTotal {
		totals.Errors[string(cat)] = uint64(max(c.Load(), 0))
	}
	if len(totals.Errors) == 0 {
		totals.Errors = nil
	}
	totals.ContractsAcquired = uint64(max(globalProm.contractsIn.Load(), 0))
	totals.ContractsDenied = uint64(max(globalProm.contractsOut.Load(), 0))
	totals.DohFailures = uint64(max(dohFailureTotal.Load(), 0))
	return totals
}

func snapshotModeCounters(s *transportModeStats) modeCounters {
	return modeCounters{
		FramesTx: s.framesTx.Load(),
		FramesRx: s.framesRx.Load(),
		BytesTx:  s.bytesTx.Load(),
		BytesRx:  s.bytesRx.Load(),
	}
}

func seedModeCounters(s *transportModeStats, c modeCounters) {
	s.framesTx.Add(c.FramesTx)
	s.framesRx.Add(c.FramesRx)
	s.bytesTx.Add(c.BytesTx)
	s.bytesRx.Add(c.BytesRx)
}

func snapshotH3Family(c *h3FamilyCounters) modeCounters {
	m := snapshotModeCounters(&c.transportModeStats)
	m.Attempts = c.attempts.Load()
	m.Connects = c.connects.Load()
	m.ConnectFailures = c.connectFailures.Load()
	m.Drops = c.drops.Load()
	return m
}

func seedH3Family(c *h3FamilyCounters, m modeCounters) {
	seedModeCounters(&c.transportModeStats, m)
	c.attempts.Add(m.Attempts)
	c.connects.Add(m.Connects)
	c.connectFailures.Add(m.ConnectFailures)
	c.drops.Add(m.Drops)
}

func snapshotDatagramCounters() datagramCounters {
	return datagramCounters{
		Offered:          h3DatagramOffered.Load(),
		Accepted:         h3DatagramAccepted.Load(),
		RxDrops:          h3DatagramDrops.Load(),
		RxMessages:       h3DatagramStats.receivedMessageCount.Load(),
		RxBytes:          h3DatagramStats.receivedMessageByteCount.Load(),
		RxMalformed:      h3DatagramStats.malformedFragmentCount.Load(),
		RxDuplicate:      h3DatagramStats.duplicateFragmentCount.Load(),
		RxChecksum:       h3DatagramStats.checksumFailureCount.Load(),
		TxMessages:       h3DatagramStats.sentMessageCount.Load(),
		TxBytes:          h3DatagramStats.sentMessageByteCount.Load(),
		TxStreamMessages: h3DatagramStats.streamSentMessageCount.Load(),
		TxErrors:         h3DatagramStats.sendErrorCount.Load(),
		Blackholes:       h3DatagramBlackholes.Load(),
	}
}

func seedDatagramCounters(c datagramCounters) {
	h3DatagramOffered.Add(c.Offered)
	h3DatagramAccepted.Add(c.Accepted)
	h3DatagramDrops.Add(c.RxDrops)
	h3DatagramStats.receivedMessageCount.Add(c.RxMessages)
	h3DatagramStats.receivedMessageByteCount.Add(c.RxBytes)
	h3DatagramStats.malformedFragmentCount.Add(c.RxMalformed)
	h3DatagramStats.duplicateFragmentCount.Add(c.RxDuplicate)
	h3DatagramStats.checksumFailureCount.Add(c.RxChecksum)
	h3DatagramStats.sentMessageCount.Add(c.TxMessages)
	h3DatagramStats.sentMessageByteCount.Add(c.TxBytes)
	h3DatagramStats.streamSentMessageCount.Add(c.TxStreamMessages)
	h3DatagramStats.sendErrorCount.Add(c.TxErrors)
	h3DatagramBlackholes.Add(c.Blackholes)
}

// addInt64Counter adds a non-negative stored total to an int64 atomic, with
// saturation: a sum past MaxInt64 clamps there instead of wrapping negative,
// which would read as a counter at zero on the next scrape.
func addInt64Counter(dst *atomic.Int64, v uint64) {
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

// raiseU64 raises a counter to want when want is ahead of it, and does nothing
// otherwise. Used by CatchUpMetricsCounters, which brings this process up to
// what another process has already written.
func raiseU64(dst *atomic.Uint64, want uint64) {
	if cur := dst.Load(); want > cur {
		dst.Add(want - cur)
	}
}

// raiseI64 is raiseU64 for a signed counter; want above MaxInt64 saturates.
func raiseI64(dst *atomic.Int64, want uint64) {
	cur := dst.Load()
	if cur < 0 {
		dst.Store(0)
		cur = 0
	}
	if want <= uint64(cur) {
		return
	}
	if want > math.MaxInt64 {
		dst.Store(math.MaxInt64)
		return
	}
	dst.Add(int64(want - uint64(cur)))
}

// --- per-proxy seeding and folding ---

// seedProxyBandwidthLocked gives a bandwidth record the totals persisted for
// its identity. Caller holds proxyHealthMu; the store lock is taken inside, so
// the lock order is always proxyHealthMu -> metricsPersistMu (no path takes
// them the other way round).
func seedProxyBandwidthLocked(key string, bw *ProxyBandwidth) {
	if bw == nil || bw.seeded || key == "" {
		return
	}
	// Only a record seeded against a real identity is marked done. A record
	// created before the key was registered stays unseeded, so the seeding
	// still happens once RegisterProxy supplies the identity.
	bw.seeded = true
	metricsPersistMu.Lock()
	base, ok := metricsProxyBase[key]
	metricsPersistMu.Unlock()
	if !ok {
		return
	}
	bw.TotalRx.Add(base.TotalRx)
	bw.TotalTx.Add(base.TotalTx)
	bw.BillableRx.Add(base.BillableRx)
	bw.BillableTx.Add(base.BillableTx)
}

// catchUpProxyBandwidth raises a live record to the totals the store has for
// its identity. Caller does not hold proxyHealthMu.
func catchUpProxyBandwidth(key string, want metricsProxyCounters) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByKey[key]
	if !ok || h.bw == nil {
		return
	}
	raiseU64(&h.bw.TotalRx, want.TotalRx)
	raiseU64(&h.bw.TotalTx, want.TotalTx)
	raiseU64(&h.bw.BillableRx, want.BillableRx)
	raiseU64(&h.bw.BillableTx, want.BillableTx)
}

// foldProxyBandwidthLocked records the final totals of a proxy that is being
// unregistered, so bytes moved since the last flush are not lost with the
// record. Caller holds proxyHealthMu.
func foldProxyBandwidthLocked(key string, bw *ProxyBandwidth) {
	if key == "" || bw == nil {
		return
	}
	live := metricsProxyCounters{
		TotalRx:    bw.TotalRx.Load(),
		TotalTx:    bw.TotalTx.Load(),
		BillableRx: bw.BillableRx.Load(),
		BillableTx: bw.BillableTx.Load(),
	}
	metricsPersistMu.Lock()
	if metricsProxyBase == nil {
		// The flush assigns a pruned map, and pruning an empty set returns
		// nil. A later fold would then write into a nil map.
		metricsProxyBase = map[string]metricsProxyCounters{}
	}
	base := metricsProxyBase[key]
	base.TotalRx = max(base.TotalRx, live.TotalRx)
	base.TotalTx = max(base.TotalTx, live.TotalTx)
	base.BillableRx = max(base.BillableRx, live.BillableRx)
	base.BillableTx = max(base.BillableTx, live.BillableTx)
	base.LastSeen = time.Now().Unix()
	metricsProxyBase[key] = base
	metricsPersistMu.Unlock()
}

func seedProxyBases(proxies map[string]metricsProxyCounters) {
	if len(proxies) == 0 {
		return
	}
	metricsPersistMu.Lock()
	defer metricsPersistMu.Unlock()
	if metricsProxyBase == nil {
		metricsProxyBase = map[string]metricsProxyCounters{}
	}
	for key, v := range proxies {
		metricsProxyBase[key] = mergeProxyCounters2(metricsProxyBase[key], v)
	}
}

// --- persistence helpers ---

// errMetricsCountersNewer reports a store written by a build with a newer
// schema. Reading stops there and the flush refuses to overwrite it.
var errMetricsCountersNewer = errors.New("metrics counters: store written by a newer build")

// readMetricsCountersFile reads and validates the store. A missing file is
// reported as fs.ErrNotExist and a newer schema as errMetricsCountersNewer, so
// callers can tell "nothing yet" from "do not touch this".
func readMetricsCountersFile(path string) (metricsCountersFile, error) {
	var file metricsCountersFile
	data, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, fmt.Errorf("parse: %w", err)
	}
	if file.Version <= 0 {
		return file, fmt.Errorf("missing schema version")
	}
	if file.Version > metricsCountersVersion {
		return file, fmt.Errorf("%w (file version %d, this build understands %d)", errMetricsCountersNewer, file.Version, metricsCountersVersion)
	}
	return file, nil
}

// writeFileAtomicFsync replaces path with data without ever truncating the
// live file: the bytes go to a temp file in the same directory, are fsynced,
// renamed over the target, and the directory is fsynced so the rename itself
// survives a power loss. A SIGKILL mid-write leaves the previous file intact.
func writeFileAtomicFsync(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	removeStaleCounterTemps(dir)
	tmp, err := os.CreateTemp(dir, ".metrics-counters-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Close and remove on every return path: a panic between here and the
	// rename would otherwise leak the descriptor and, on Windows, leave the
	// temp file undeletable.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp already opens at 0600, which is the mode this store wants. A
	// filesystem that cannot chmod (CIFS, FAT) must not fail the write.
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	// Best effort: a directory fsync is what makes the rename durable, but the
	// call is unavailable or meaningless on some platforms. The data is
	// already fsynced; failing here would report a write that did happen.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// removeStaleCounterTemps deletes temp files a killed writer left behind. Only
// files older than ten minutes are touched, so a concurrent process's
// in-progress write is never removed under it.
func removeStaleCounterTemps(dir string) {
	stale, err := filepath.Glob(filepath.Join(dir, ".metrics-counters-*.tmp"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, path := range stale {
		if info, err := os.Stat(path); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

// mergeCounterTotals takes the larger of each pair. Every field is a monotonic
// total, so max() is the correct merge for two writers.
func mergeCounterTotals(a, b metricsCounterTotals) metricsCounterTotals {
	return metricsCounterTotals{
		H1:                mergeModeCounters(a.H1, b.H1),
		H1Direct:          mergeModeCounters(a.H1Direct, b.H1Direct),
		H3:                mergeModeCounters(a.H3, b.H3),
		H3Dns:             mergeModeCounters(a.H3Dns, b.H3Dns),
		H3DnsPump:         mergeModeCounters(a.H3DnsPump, b.H3DnsPump),
		H3Datagram:        mergeDatagramCounters(a.H3Datagram, b.H3Datagram),
		Errors:            mergeCounterErrors(a.Errors, b.Errors),
		ContractsAcquired: max(a.ContractsAcquired, b.ContractsAcquired),
		ContractsDenied:   max(a.ContractsDenied, b.ContractsDenied),
		DohFailures:       max(a.DohFailures, b.DohFailures),
	}
}

func mergeModeCounters(a, b modeCounters) modeCounters {
	return modeCounters{
		FramesTx:        max(a.FramesTx, b.FramesTx),
		FramesRx:        max(a.FramesRx, b.FramesRx),
		BytesTx:         max(a.BytesTx, b.BytesTx),
		BytesRx:         max(a.BytesRx, b.BytesRx),
		Attempts:        max(a.Attempts, b.Attempts),
		Connects:        max(a.Connects, b.Connects),
		ConnectFailures: max(a.ConnectFailures, b.ConnectFailures),
		Drops:           max(a.Drops, b.Drops),
	}
}

func mergeDatagramCounters(a, b datagramCounters) datagramCounters {
	return datagramCounters{
		Offered:          max(a.Offered, b.Offered),
		Accepted:         max(a.Accepted, b.Accepted),
		RxDrops:          max(a.RxDrops, b.RxDrops),
		RxMessages:       max(a.RxMessages, b.RxMessages),
		RxBytes:          max(a.RxBytes, b.RxBytes),
		RxMalformed:      max(a.RxMalformed, b.RxMalformed),
		RxDuplicate:      max(a.RxDuplicate, b.RxDuplicate),
		RxChecksum:       max(a.RxChecksum, b.RxChecksum),
		TxMessages:       max(a.TxMessages, b.TxMessages),
		TxBytes:          max(a.TxBytes, b.TxBytes),
		TxStreamMessages: max(a.TxStreamMessages, b.TxStreamMessages),
		TxErrors:         max(a.TxErrors, b.TxErrors),
		Blackholes:       max(a.Blackholes, b.Blackholes),
	}
}

// mergeExtraCounters merges two keyed counters with the same max rule.
func mergeExtraCounters(a, b map[string]uint64) map[string]uint64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = max(out[k], v)
	}
	return out
}

// mergeCounterErrors merges the error categories this build knows, dropping
// any key the running code does not emit. A category that was removed in a
// newer build, or written by hand, would otherwise be re-serialized forever.
func mergeCounterErrors(known, stored map[string]uint64) map[string]uint64 {
	if len(known) == 0 {
		return mergeExtraCounters(nil, stored)
	}
	out := make(map[string]uint64, len(known))
	for k, v := range known {
		out[k] = v
	}
	for k, v := range stored {
		if _, ok := known[k]; !ok {
			continue
		}
		out[k] = max(out[k], v)
	}
	return out
}

// mergeExtraSources merges the per-source maps with the same max rule, so a
// source that has not registered yet in this process (a loop that has not been
// started, say) keeps the totals the previous process wrote for it instead of
// disappearing from the store.
func mergeExtraSources(a, b map[string]map[string]uint64) map[string]map[string]uint64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]map[string]uint64, len(a)+len(b))
	for name, vals := range a {
		out[name] = mergeExtraCounters(out[name], vals)
	}
	for name, vals := range b {
		out[name] = mergeExtraCounters(out[name], vals)
	}
	return out
}

func mergeProxyCounters2(a, b metricsProxyCounters) metricsProxyCounters {
	return metricsProxyCounters{
		TotalRx:    max(a.TotalRx, b.TotalRx),
		TotalTx:    max(a.TotalTx, b.TotalTx),
		BillableRx: max(a.BillableRx, b.BillableRx),
		BillableTx: max(a.BillableTx, b.BillableTx),
		LastSeen:   max(a.LastSeen, b.LastSeen),
	}
}

// mergeProxyMaps merges two per-identity maps with max-per-field. It does not
// touch LastSeen; the caller stamps the identities that are live right now.
func mergeProxyMaps(a, b map[string]metricsProxyCounters) map[string]metricsProxyCounters {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]metricsProxyCounters, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = mergeProxyCounters2(out[k], v)
	}
	return out
}

// pruneProxyCounters drops entries that have been absent longer than the
// retention window, then enforces the hard cap by last-seen order. live marks
// the identities the node is serving right now; those are never dropped, even
// when the live set alone exceeds the cap.
func pruneProxyCounters(all, live map[string]metricsProxyCounters, now time.Time) map[string]metricsProxyCounters {
	if len(all) == 0 {
		return nil
	}
	nowUnix := now.Unix()
	cutoff := now.Add(-metricsProxyRetention).Unix()
	out := make(map[string]metricsProxyCounters, len(all))
	for k, v := range all {
		// A timestamp in the future (clock jump, edited file) would otherwise
		// pin the entry permanently: it never ages past the cutoff and always
		// sorts last in the eviction queue.
		if v.LastSeen > nowUnix {
			v.LastSeen = nowUnix
		}
		if _, isLive := live[k]; isLive {
			out[k] = v
			continue
		}
		if v.LastSeen < cutoff {
			continue
		}
		out[k] = v
	}
	if len(out) <= metricsProxyMaxEntries {
		return out
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ki, kj := keys[i], keys[j]
		_, li := live[ki]
		_, lj := live[kj]
		if li != lj {
			return !li // dead entries sort first, so they are dropped first
		}
		return out[ki].LastSeen < out[kj].LastSeen
	})
	// Only drop dead entries. A live set larger than the cap keeps every live
	// proxy; the cap exists to bound churn, not to delete what the node serves.
	dead := len(out) - len(live)
	if dead < 0 {
		dead = 0
	}
	drop := len(out) - metricsProxyMaxEntries
	if drop > dead {
		drop = dead
	}
	for _, k := range keys[:drop] {
		delete(out, k)
	}
	return out
}
