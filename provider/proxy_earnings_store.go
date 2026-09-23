package main

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// SAFETY: the launch ranking now reads scores to order proxies by
// earnings. The store collects, persists, and reports the history, and
// the scheduler consumes it to break ties within the same warmth tier
// and provenance group. The store never admits, evicts, or rejects a proxy.
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

	live := make(map[string]struct{}, len(snapshot))
	for key, bw := range snapshot {
		// Snapshot keys arrive in two shapes: the display format
		// "proxy[N] (addr)" (ProxyHealthSnapshot) or an identity key
		// (ProxyBandwidthSnapshotByKey, raw addresses). Only the display
		// format needs normalizing down to its address; identity keys are
		// already in store key space and must pass through untouched —
		// collapsing them to a bare address would recreate the legacy-key
		// collision this store migrated away from.
		credKey := key
		if strings.HasPrefix(key, "proxy[") {
			credKey = proxyKeyAddress(key)
		}
		live[credKey] = struct{}{}
		cum := bw.BillableRx.Load() + bw.BillableTx.Load()
		prev, seen := s.prevCum[credKey]
		s.prevCum[credKey] = cum
		if !seen || cum <= prev {
			continue
		}
		s.creditLocked(credKey, float64(cum-prev), now)
	}

	// Drop baselines for addresses that left the snapshot. Without this the
	// map gains an entry for every address ever seen and loses none, which
	// on a node churning thousands of URL-sourced proxies grows for the life
	// of the process. perProxyEarnTracker prunes for the same reason.
	//
	// Only the baseline is pruned, never the earnings record: the record is
	// the point of this store and an offline proxy still has one. Dropping
	// the baseline costs nothing, because a proxy that returns comes back
	// with counters reset to zero, which re-baselines on first sight anyway.
	for addr := range s.prevCum {
		if _, ok := live[addr]; !ok {
			delete(s.prevCum, addr)
		}
	}
}

// adoptLegacy migrates a legacy (bare-address-keyed) earnings entry to its
// new identity key (ProxySettings.Key()) for every address the given
// desired settings actually claim, mirroring adoptLegacyProxyState's rules
// exactly (see its doc comment) — this file has no separate Version field
// because its on-disk shape is a bare {address: entry} map, not a wrapped
// struct, so adding one would be a breaking format change this migration
// does not need: adoption is entirely inferrable from the map's own keys,
// with no schema flag required.
//
// s.prevCum is deliberately NOT touched here: it is never persisted (see
// its field doc comment), so there is no legacy prevCum data to migrate —
// it is always empty at process start and only ever populated by Observe()
// from a live snapshot, which will already carry the correct identity key
// once the reload engine is wired up.
func (s *proxyEarningsStore) adoptLegacy(desired []*connect.ProxySettings) (adopted, split int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byAddress := make(map[string][]*connect.ProxySettings, len(desired))
	for _, settings := range desired {
		if settings == nil || settings.Address == "" {
			continue
		}
		byAddress[settings.Address] = append(byAddress[settings.Address], settings)
	}

	for address, settingsAtAddress := range byAddress {
		legacy, hasLegacy := s.entries[address]
		if !hasLegacy {
			continue
		}

		seen := map[string]bool{}
		var keys []string
		for _, settings := range settingsAtAddress {
			k := settings.Key()
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		winner := keys[0]

		if winner == address {
			continue
		}

		delete(s.entries, address)
		if existing, ok := s.entries[winner]; ok {
			// Both entries describe DISJOINT earning periods: the legacy
			// (bare-address) entry predates the identity-keyed ingest, and
			// the winner entry holds post-upgrade credits. Merge additively,
			// decayed to the later timestamp, so neither side's earned
			// bytes are lost — replacing would erase accumulated score.
			if existing.Updated.After(legacy.Updated) {
				existing.Score += decayEarningsScore(legacy.Score, existing.Updated.Sub(legacy.Updated))
				s.entries[winner] = existing
			} else {
				legacy.Score += decayEarningsScore(existing.Score, legacy.Updated.Sub(existing.Updated))
				s.entries[winner] = legacy
			}
		} else {
			s.entries[winner] = legacy
		}
		adopted++

		if len(keys) > 1 {
			split++
			tlog("[proxy][identity] earnings for %s split into %d identities on adoption; %s kept its history, the rest start at zero\n",
				address, len(keys), proxyKeyDisplay(winner))
		}
	}
	return adopted, split
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
		live  bool
	}
	list := make([]ranked, 0, len(s.entries))
	for addr, e := range s.entries {
		score := decayEarningsScore(e.Score, now.Sub(e.Updated))
		if score < earningsMinRetainedScore {
			continue
		}
		_, live := s.prevCum[addr]
		list = append(list, ranked{addr, proxyEarningsEntry{Score: score, Updated: now}, live})
	}

	// Live proxies outrank every offline one, then score, then address for
	// a deterministic tiebreak.
	//
	// Ranking by score alone lets a long-dead proxy holding a large decayed
	// burst push out a live proxy earning steadily. That is not just a lost
	// record: the evicted proxy's baseline survives in prevCum because it
	// is still live, so the next tick creates a fresh entry worth one
	// tick of traffic, its score is then tiny, and the next save evicts it
	// again. It resets every save interval while dead proxies hold the
	// ranking, which is the exact opposite of what this store is for.
	//
	// Observe prunes prevCum against the live snapshot, so its key set is
	// the live proxy set and is the right thing to ask.
	sort.Slice(list, func(i, j int) bool {
		if list[i].live != list[j].live {
			return list[i].live
		}
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

	// prevCum is pruned by Observe against the live snapshot, not here, so
	// an address evicted from the history but still serving keeps its
	// baseline and does not re-credit its whole cumulative total.
	path := s.path
	s.mu.Unlock()

	return atomicWriteJSON(path, out)
}

// earningsPromotionBytes is the decayed score at which a URL-sourced proxy
// stops being treated as an unproven address and is ordered alongside the
// file list. It is an absolute floor rather than a percentile: on a node
// where nothing earns, nothing should be promoted, and a relative bar would
// always promote the least-bad address.
const earningsPromotionBytes = 64 << 20 // 64 MiB

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
// all, how many URL-sourced proxies have been promoted, and the single
// biggest earner.
func earningsHistorySummary(
	proxies []*connect.ProxySettings,
	proxySourceOf map[string]string,
	now time.Time,
) (ranked int, promoted int, topAddr string, topScore float64) {
	for _, p := range proxies {
		score := proxyEarningsScore(p.Key(), now)
		// Same cutoff Save uses, so the line cannot count a sub-byte
		// residue as "has earned" that the next save will drop.
		if score < earningsMinRetainedScore {
			continue
		}
		ranked++
		if proxySourceOf[p.Key()] == "url" && score >= earningsPromotionBytes {
			promoted++
		}
		if score > topScore {
			topScore = score
			topAddr = p.Address
		}
	}
	return ranked, promoted, topAddr, topScore
}
