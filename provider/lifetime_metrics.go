package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// lifetimeMetrics persists all-time counters across provider restarts.
//
// Everything here already exists as an in-memory counter somewhere (PQE
// opens, contracts acquired/denied, proxy recovered/lost transitions,
// cumulative billable); the problem was purely that process death erased
// them. This store holds the ALL-TIME totals: loaded once at startup as
// the pre-restart base, incremented by DELTA observations from the periodic
// collectors (never absolute assignments — a counter reset between ticks
// must not corrupt history), and flushed to disk throttled.
//
// File format is JSON under ~/.urnetwork/ (same home for fast_auth and
// pressure_status). Writes are atomic: temp file + rename, so a
// crash mid-write can never leave a half-written store. Flushes happen on a
// throttle (min interval between disk writes) plus a final flush on context
// cancellation; worst case a hard kill loses at most one throttle window of
// deltas — acceptable for operator-visible lifetime stats.
type lifetimeMetrics struct {
	mu sync.Mutex

	// Persistent totals (the authoritative numbers).
	PQEOpens      uint64 `json:"pqe_opens"`
	ClasOpens     uint64 `json:"clas_opens"`
	ContractsUp   uint64 `json:"contracts_acquired"`
	ContractsDeny uint64 `json:"contracts_denied"`
	ProxiesRecov  uint64 `json:"proxies_recovered"`
	ProxiesLost   uint64 `json:"proxies_lost"`
	BillableBytes uint64 `json:"billable_bytes"`

	// Since is when these totals began accumulating, so a log line can say
	// what "all-time" actually covers. It is written once, when the store is
	// created, and never rewritten afterwards.
	//
	// SinceApprox marks a store that predates this field: its totals really
	// do start somewhere earlier, but nothing recorded where, so the start is
	// pinned to the first load that understood the field.
	Since       time.Time `json:"since,omitempty"`
	SinceApprox bool      `json:"since_approx,omitempty"`

	path       string
	dirty      bool
	lastFlush  time.Time
	flushEvery time.Duration
}

// lifetimeMetricsPath returns ~/.urnetwork/lifetime_metrics.json (created on
// first save).
func lifetimeMetricsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".urnetwork", "lifetime_metrics.json")
}

// loadLifetimeMetrics reads the persisted store (empty store when absent or
// unreadable — a corrupt file resets counters rather than blocking the
// provider; these are operator-visible stats, not consensus state).
func loadLifetimeMetrics(path string) *lifetimeMetrics {
	lm := &lifetimeMetrics{path: path, flushEvery: 5 * time.Minute, lastFlush: time.Now(), Since: time.Now()}
	if path == "" {
		return lm
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return lm // first boot or unreadable: start fresh, so "since" is now
	}
	var persisted struct {
		PQEOpens      uint64 `json:"pqe_opens"`
		ClasOpens     uint64 `json:"clas_opens"`
		ContractsUp   uint64 `json:"contracts_acquired"`
		ContractsDeny uint64 `json:"contracts_denied"`
		ProxiesRecov  uint64 `json:"proxies_recovered"`
		ProxiesLost   uint64 `json:"proxies_lost"`
		BillableBytes uint64 `json:"billable_bytes"`
		SavedAt       string `json:"saved_at"`
		Since         string `json:"since"`
		SinceApprox   bool   `json:"since_approx"`
	}
	if json.Unmarshal(data, &persisted) != nil {
		return lm
	}
	lm.PQEOpens = persisted.PQEOpens
	lm.ClasOpens = persisted.ClasOpens
	lm.ContractsUp = persisted.ContractsUp
	lm.ContractsDeny = persisted.ContractsDeny
	lm.ProxiesRecov = persisted.ProxiesRecov
	lm.ProxiesLost = persisted.ProxiesLost
	lm.BillableBytes = persisted.BillableBytes
	// A store written before the field existed keeps its real totals but has
	// no recorded start: pin the start to now and say so, rather than
	// claiming a date the numbers do not cover.
	if since, perr := time.Parse(time.RFC3339, persisted.Since); perr == nil && !since.IsZero() {
		lm.Since = since
		lm.SinceApprox = persisted.SinceApprox
	} else {
		lm.SinceApprox = true
		// Persist the pinned start even if no counter ever moves: otherwise an
		// idle node never flushes, and every restart re-pins a slightly later
		// date while the totals stay put.
		lm.dirty = true
	}
	// Anchor the flush throttle to the last real save (not boot time), so a
	// quick restart doesn't delay the first persistence of new deltas by a
	// full throttle window after an already-stale file.
	if savedAt, perr := time.Parse(time.RFC3339, persisted.SavedAt); perr == nil {
		lm.lastFlush = savedAt
	}
	return lm
}

// Started reports when the lifetime totals began and whether that start is
// approximate. A false approximation means the date is exact; true means the
// totals include history from before this build recorded one.
func (lm *lifetimeMetrics) Started() (since time.Time, approx bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.Since, lm.SinceApprox
}

// sinceLabel renders a start time for an operator-facing log line, e.g.
// "since 2026-10-05 08:12 PDT". An approximate start says why it is one.
func sinceLabel(t time.Time, approx bool) string {
	if t.IsZero() {
		return "since unknown"
	}
	label := "since " + clockLabel(t)
	if approx {
		label += ", start approximate, totals include earlier history"
	}
	return label
}

// clockLabel renders an instant the way the operator-facing lines show it:
// local time with the zone, so "when did this start" needs no arithmetic.
func clockLabel(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

// Add applies positive deltas to named totals and marks the store dirty.
// Callers pass reset-guarded deltas; a fully-zero call is a cheap no-op
// that does NOT mark the store dirty (so idle nodes never rewrite the
// state file).
func (lm *lifetimeMetrics) Add(pqeOpens, clasOpens, contractsUp, contractsDeny, proxiesRecov, proxiesLost uint64, billableBytesDelta uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if pqeOpens|clasOpens|contractsUp|contractsDeny|proxiesRecov|proxiesLost|billableBytesDelta == 0 {
		return
	}
	lm.PQEOpens += pqeOpens
	lm.ClasOpens += clasOpens
	lm.ContractsUp += contractsUp
	lm.ContractsDeny += contractsDeny
	lm.ProxiesRecov += proxiesRecov
	lm.ProxiesLost += proxiesLost
	lm.BillableBytes += billableBytesDelta
	lm.dirty = true
}

// MaybeFlush writes the store to disk if it is dirty and the flush throttle
// has elapsed. Returns whether a write happened.
func (lm *lifetimeMetrics) MaybeFlush(now time.Time) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.dirty || lm.path == "" {
		return false
	}
	if !lm.lastFlush.IsZero() && now.Sub(lm.lastFlush) < lm.flushEvery {
		return false
	}
	if err := lm.writeLocked(); err != nil {
		return false // keep dirty; retry next tick
	}
	lm.dirty = false
	lm.lastFlush = now
	return true
}

// Flush forces a write (shutdown path).
func (lm *lifetimeMetrics) Flush() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.dirty || lm.path == "" {
		return
	}
	if lm.writeLocked() == nil {
		lm.dirty = false
	}
}

// writeLocked performs the atomic temp+rename write. Caller holds mu.
func (lm *lifetimeMetrics) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(lm.path), 0o700); err != nil {
		return err
	}
	// A zero Since must stay empty: time.Time marshals its zero value as
	// year 1, which would come back as an exact start on the next load.
	sinceValue := ""
	if !lm.Since.IsZero() {
		sinceValue = lm.Since.UTC().Format(time.RFC3339)
	}
	blob, err := json.MarshalIndent(struct {
		PQEOpens      uint64 `json:"pqe_opens"`
		ClasOpens     uint64 `json:"clas_opens"`
		ContractsUp   uint64 `json:"contracts_acquired"`
		ContractsDeny uint64 `json:"contracts_denied"`
		ProxiesRecov  uint64 `json:"proxies_recovered"`
		ProxiesLost   uint64 `json:"proxies_lost"`
		BillableBytes uint64 `json:"billable_bytes"`
		SavedAt       string `json:"saved_at"`
		Since         string `json:"since,omitempty"`
		SinceApprox   bool   `json:"since_approx,omitempty"`
	}{
		PQEOpens:      lm.PQEOpens,
		ClasOpens:     lm.ClasOpens,
		ContractsUp:   lm.ContractsUp,
		ContractsDeny: lm.ContractsDeny,
		ProxiesRecov:  lm.ProxiesRecov,
		ProxiesLost:   lm.ProxiesLost,
		BillableBytes: lm.BillableBytes,
		SavedAt:       time.Now().UTC().Format(time.RFC3339),
		Since:         sinceValue,
		SinceApprox:   lm.SinceApprox,
	}, "", " ")
	if err != nil {
		return err
	}
	tmp := lm.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, lm.path)
}

// Snapshot returns a copy of the current totals for logging.
func (lm *lifetimeMetrics) Snapshot() (pqe, clas, up, deny, recov, lost, bytes uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.PQEOpens, lm.ClasOpens, lm.ContractsUp, lm.ContractsDeny,
		lm.ProxiesRecov, lm.ProxiesLost, lm.BillableBytes
}
