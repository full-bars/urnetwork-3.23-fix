package main

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Per-proxy earnings history (design note 2026-09-11).
//
// The provider already knows, at every heartbeat, how many billable bytes
// each proxy has moved. That signal is spent immediately: it is summed for
// the hourly usage row, rendered into the traffic report, and reduced to a
// last-earned timestamp by perProxyEarnTracker. None of it survives a
// restart, so the launch scheduler has never been able to ask "which of
// these identities actually earns".
//
// This store answers that question. It keeps one exponentially decaying
// score per proxy address, in bytes, with a one-week half-life: a proxy
// that moved a gigabyte yesterday outranks one that moved a gigabyte two
// months ago, and a proxy that stops earning falls off on its own without
// any rotation or pruning pass.
//
// It deliberately does NOT prune addresses that leave the live snapshot.
// perProxyEarnTracker must prune, because it answers "is this proxy
// earning right now". This store answers "has this identity earned", which
// is precisely the question an offline proxy still has an answer to.
//
// SAFETY: nothing reads the score to make a decision yet. This change
// collects and persists the history and reports it at startup; the launch
// ranking that will consume it is deliberately held back so it can be
// judged against a real week of data. The store never admits, evicts, or
// rejects a proxy.
const (
	// earningsHalfLife is how long a score takes to fall to half its value
	// with no further earnings. A week is long enough to ride out a quiet
	// weekend and short enough that a month-dead proxy ranks near zero.
	earningsHalfLife = 7 * 24 * time.Hour

	// earningsDefaultMaxEntries bounds the file. A node carrying several
	// thousand proxies churns through many more addresses over its life,
	// and the lowest scorers are the ones worth forgetting.
	earningsDefaultMaxEntries = 20000

	// earningsMinRetainedScore drops sub-byte decayed residue on save, so
	// an address that earned once a year ago does not occupy a slot that a
	// current earner could use.
	earningsMinRetainedScore = 1.0

	// earningsSaveInterval bounds how often the store is written. The
	// snapshot loop ticks once a minute, and the ranking only has to
	// survive a restart, so writing on every tick would be pure write
	// amplification on nodes whose state dir is on flash.
	earningsSaveInterval = 15 * time.Minute
)

// proxyEarningsEntry is one proxy's decayed earnings score and the moment
// that score was last brought up to date. Score is in bytes.
type proxyEarningsEntry struct {
	Score   float64   `json:"score"`
	Updated time.Time `json:"updated"`
}

type proxyEarningsStore struct {
	mu   sync.Mutex
	path string

	// maxEntries overrides earningsDefaultMaxEntries when positive. Tests
	// set it directly; production leaves it zero.
	maxEntries int

	entries map[string]*proxyEarningsEntry

	// lastSave is when MaybeSave last wrote, for the save throttle.
	lastSave time.Time

	// prevCum holds the previous cumulative billable total per address for
	// the delta computation. It is NOT persisted: the counters it mirrors
	// are process-lifetime atomics that reset to zero on restart, so a
	// restored baseline would read as a counter reset on the first tick.
	prevCum map[string]uint64
}

func newProxyEarningsStore(path string) *proxyEarningsStore {
	return &proxyEarningsStore{
		path:    path,
		entries: map[string]*proxyEarningsEntry{},
		prevCum: map[string]uint64{},
	}
}

// decayEarningsScore applies the half-life decay for an elapsed duration.
// A non-positive elapsed time (a clock stepping backwards) leaves the score
// alone rather than amplifying it.
func decayEarningsScore(score float64, elapsed time.Duration) float64 {
	if score == 0 || elapsed <= 0 {
		return score
	}
	return score * math.Exp2(-elapsed.Hours()/earningsHalfLife.Hours())
}

// Observe folds one bandwidth snapshot into the store, crediting each proxy
// with the billable bytes it has moved since the previous call.
//
// An address seen for the first time only establishes its baseline: the
// cumulative counter it arrives with was earned before this store was
// watching. A counter that moves backwards means the proxy restarted, which
// re-baselines without crediting or debiting anything.
func (s *proxyEarningsStore) Observe(snapshot map[string]*connect.ProxyBandwidth, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, bw := range snapshot {
		addr := proxyKeyAddress(key)
		cum := bw.BillableRx.Load() + bw.BillableTx.Load()
		prev, seen := s.prevCum[addr]
		s.prevCum[addr] = cum
		if !seen || cum <= prev {
			continue
		}
		s.creditLocked(addr, float64(cum-prev), now)
	}
}

func (s *proxyEarningsStore) creditLocked(addr string, delta float64, now time.Time) {
	e, ok := s.entries[addr]
	if !ok {
		e = &proxyEarningsEntry{Updated: now}
		s.entries[addr] = e
	}
	e.Score = decayEarningsScore(e.Score, now.Sub(e.Updated)) + delta
	e.Updated = now
}

// Score returns the proxy's decayed earnings in bytes as of now. An address
// the store has never credited scores zero.
func (s *proxyEarningsStore) Score(addr string, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[proxyKeyAddress(addr)]
	if !ok {
		return 0
	}
	return decayEarningsScore(e.Score, now.Sub(e.Updated))
}

// Load reads the store from disk. A missing file is not an error: a node
// that has never earned simply starts with no history. Corruption falls
// back to the .bak copy and then to an empty history, because losing the
// ranking is never worth refusing to start.
func (s *proxyEarningsStore) Load() error {
	var decoded map[string]proxyEarningsEntry
	ok, err := loadJSONWithRecovery(s.path, &decoded)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for addr, e := range decoded {
		entry := e
		s.entries[addr] = &entry
	}
	return nil
}

// MaybeSave writes the store if the save interval has elapsed since the
// last write, reporting whether it wrote. A store with no configured path
// never writes: persistence is disabled, but the in-memory ranking keeps
// working for the life of the process.
//
// A save failure still counts as an attempt, so a store on a full or
// read-only disk retries on the next interval rather than on every tick.
func (s *proxyEarningsStore) MaybeSave(now time.Time) bool {
	if s.path == "" {
		return false
	}

	s.mu.Lock()
	if !s.lastSave.IsZero() && now.Sub(s.lastSave) < earningsSaveInterval {
		s.mu.Unlock()
		return false
	}
	s.lastSave = now
	s.mu.Unlock()

	if err := s.Save(now); err != nil {
		tlog("⚠️ [earn] could not persist proxy earnings history: %v\n", err)
	}
	return true
}

// Save brings every score up to date, drops the entries worth forgetting,
// and writes the result atomically. The eviction applies to the in-memory
// map as well, so a long-lived process stays bounded by the same cap as the
// file.
func (s *proxyEarningsStore) Save(now time.Time) error {
	s.mu.Lock()

	max := s.maxEntries
	if max <= 0 {
		max = earningsDefaultMaxEntries
	}

	type ranked struct {
		addr  string
		entry proxyEarningsEntry
	}
	list := make([]ranked, 0, len(s.entries))
	for addr, e := range s.entries {
		score := decayEarningsScore(e.Score, now.Sub(e.Updated))
		if score < earningsMinRetainedScore {
			continue
		}
		list = append(list, ranked{addr, proxyEarningsEntry{Score: score, Updated: now}})
	}

	// Highest scorers survive the cap. The address tiebreak keeps the
	// eviction deterministic when scores collide.
	sort.Slice(list, func(i, j int) bool {
		if list[i].entry.Score != list[j].entry.Score {
			return list[i].entry.Score > list[j].entry.Score
		}
		return list[i].addr < list[j].addr
	})
	if len(list) > max {
		list = list[:max]
	}

	out := make(map[string]proxyEarningsEntry, len(list))
	retained := make(map[string]*proxyEarningsEntry, len(list))
	for _, r := range list {
		entry := r.entry
		out[r.addr] = entry
		retained[r.addr] = &entry
	}
	s.entries = retained

	// prevCum is keyed by the live proxy set, not by the retained history,
	// so an evicted address that is still serving keeps its baseline and
	// does not re-credit its whole cumulative total on the next tick.
	path := s.path
	s.mu.Unlock()

	return atomicWriteJSON(path, out)
}

// globalProxyEarningsStore is fed by the same snapshot loop that feeds
// globalPerProxyEarnTracker and consulted by the launch scheduler.
var globalProxyEarningsStore = newProxyEarningsStore(proxyEarningsPath())

// proxyEarningsPath returns ~/.urnetwork/proxy_earnings.json. An
// unresolvable home yields an empty path, which disables persistence while
// leaving the in-memory ranking working for the life of the process.
func proxyEarningsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".urnetwork", "proxy_earnings.json")
}

// proxyEarningsScore returns the decayed earnings for addr from the global
// store, or zero when the store is unset.
func proxyEarningsScore(addr string, now time.Time) float64 {
	if globalProxyEarningsStore == nil {
		return 0
	}
	return globalProxyEarningsStore.Score(addr, now)
}

// earningsHistorySummary describes the earnings history behind a launch
// set, for the startup log: how many of the proxies carry any history at
// all, and the single biggest earner.
//
// Nothing consults the history to order launches yet. It is reported so an
// operator can watch it fill in, and so the ranking that will use it can be
// judged against real data rather than a hypothesis.
func earningsHistorySummary(
	proxies []*connect.ProxySettings,
	now time.Time,
) (ranked int, topAddr string, topScore float64) {
	for _, p := range proxies {
		score := proxyEarningsScore(p.Address, now)
		if score <= 0 {
			continue
		}
		ranked++
		if score > topScore {
			topScore = score
			topAddr = p.Address
		}
	}
	return ranked, topAddr, topScore
}
