package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// clientJWTStaleAfter prunes entries for proxies that haven't reconnected in
// this long — mostly relevant to URL-sourced proxies, whose addresses churn
// as dead entries get evicted and replaced.
const clientJWTStaleAfter = 30 * 24 * time.Hour

// clientJWTMaxBackups is how many pre-restart snapshots to keep. Older
// backups are pruned automatically when a new snapshot is created.
const clientJWTMaxBackups = 10

// clientJWTSnapshotMinInterval is the minimum wall time between snapshots of
// the same content. This prevents a crash-looping provider from cycling
// through all backup slots with near-identical crash-loop snapshots and
// evicting the pre-incident state the operator would need for recovery.
const clientJWTSnapshotMinInterval = 5 * time.Minute

type clientJWTEntry struct {
	ByClientJWT string    `json:"by_client_jwt"`
	ClientID    string    `json:"client_id"`
	NetworkID   string    `json:"network_id"`
	MintedAt    time.Time `json:"minted_at"`
}

// clientJWTStore persists each proxy's minted client JWT across process
// restarts, keyed by proxy address (or "direct" for the native connection),
// so a restart reuses an existing client_id instead of minting a fresh one
// and resetting that identity's server-side reliability reputation.
type clientJWTStore struct {
	mu      sync.Mutex
	path    string
	loaded  bool
	entries map[string]clientJWTEntry
}

func newClientJWTStore(path string) *clientJWTStore {
	return &clientJWTStore{path: path, entries: map[string]clientJWTEntry{}}
}

// globalClientJWTStore is created lazily so init never panics on a missing
// HOME. The release binary must work for --version/--help and one-shot
// commands in a bare environment (root, no HOME set). When HOME is
// unavailable, the store degrades to an in-memory-only store: identity
// reuse is lost for that process, but the command still runs. Same
// semantics as the load-error path below.
// NOTE: no tlog here — package-var init runs before output plumbing is
// set up, so any stdout here would prepend to EVERY invocation's output
// and break callers that parse it (e.g. '--version 2>&1 | head -1').
var globalClientJWTStore = func() *clientJWTStore {
	home, err := os.UserHomeDir()
	if err != nil {
		return newClientJWTStore("")
	}
	return newClientJWTStore(filepath.Join(home, ".urnetwork", ".client_jwts.json"))
}()

func (s *clientJWTStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true

	data, err := os.ReadFile(s.path)
	if err != nil {
		// Missing or unreadable: treat as an empty store. Every identity
		// mints fresh, same as before this feature existed.
		return
	}

	var entries map[string]clientJWTEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Snapshot the raw bytes before we toss them — the operator can
		// inspect a corrupt file or hand it to a recovery tool.
		s.snapshotLocked(data, 0)
		tlog("⚠️ [jwt-store] corrupt %s, starting fresh: %v\n", s.path, err)
		return
	}

	// Snapshot pre-restart state before any prune or overwrite — if every
	// entry gets replaced during startup, the snapshot is the only copy of
	// the previous identity set the operator had before this restart.
	s.snapshotLocked(data, len(entries))

	now := time.Now()
	pruned := make(map[string]clientJWTEntry, len(entries))
	for key, entry := range entries {
		if now.Sub(entry.MintedAt) < clientJWTStaleAfter {
			pruned[key] = entry
		}
	}
	s.entries = pruned

	// Log how many identities we're carrying forward — gives the operator a
	// quick signal at startup whether hot-restart will reuse (the goal) or
	// whether a restart will mint fresh (and why they should care).
	total := len(entries)
	kept := len(pruned)
	if total == 0 {
		tlog("🔥 [hot-restart] no stored client identities found; all proxies will mint fresh on first auth\n")
	} else if kept < total {
		tlog("🔥 [hot-restart] loaded %d stored identities (%d pruned as stale >30d); carrying forward %d\n", total, total-kept, kept)
	} else {
		tlog("🔥 [hot-restart] loaded %d stored identities; carrying forward all %d\n", total, kept)
	}
}

// snapshotLocked writes a timestamped copy of the raw store bytes BEFORE any
// prune/overwrite, so a destructive restart (e.g. a code change that remints
// every client_id) leaves the operator a recoverable copy of the prior
// identity set. It is a no-op in in-memory-only mode. Dedup: if the most
// recent backup has identical content AND was written within
// clientJWTSnapshotMinInterval, we skip — this stops a crash-loop from
// cycling through all backup slots with near-identical snapshots and evicting
// the pre-incident state.
func (s *clientJWTStore) snapshotLocked(data []byte, count int) {
	if s.path == "" {
		return
	}
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)

	// Dedup against the most recent backup.
	if entries, err := filepath.Glob(filepath.Join(dir, base+"-*.bak")); err == nil && len(entries) > 0 {
		sort.Strings(entries) // oldest first; last element is newest
		newest := entries[len(entries)-1]
		if prev, err := os.ReadFile(newest); err == nil {
			if bytes.Equal(prev, data) {
				// Identical content — only skip if it's recent enough that
				// this isn't the first snapshot after a long gap.
				if info, err := os.Stat(newest); err == nil {
					if time.Since(info.ModTime()) < clientJWTSnapshotMinInterval {
						return
					}
				}
			}
		}
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	bak := filepath.Join(dir, fmt.Sprintf("%s-%s.bak", base, ts))
	// Atomically write via tmp+rename (same as flushLocked) so a crash
	// mid-write — the exact scenario clientJWTSnapshotMinInterval guards
	// against — can't leave a truncated backup behind.
	tmp := bak + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		tlog("⚠️ [jwt-store] snapshot to %s failed: %v\n", bak, err)
		return
	}
	if err := os.Rename(tmp, bak); err != nil {
		// Best-effort: drop the tmp half so it doesn't accumulate.
		_ = os.Remove(tmp)
		tlog("⚠️ [jwt-store] snapshot to %s failed: %v\n", bak, err)
		return
	}
	if count > 0 {
		tlog("📸 [jwt-store] snapshot saved to %s (%d entries)\n", bak, count)
	} else {
		tlog("📸 [jwt-store] snapshot saved to %s (empty store)\n", bak)
	}
	pruneOldBackupsLocked(dir, base)
}

// pruneOldBackupsLocked keeps only the newest clientJWTMaxBackups snapshots
// for the given store base name.
func pruneOldBackupsLocked(dir, base string) {
	entries, err := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if err != nil || len(entries) <= clientJWTMaxBackups {
		return
	}
	sort.Strings(entries) // oldest first
	for _, old := range entries[:len(entries)-clientJWTMaxBackups] {
		if err := os.Remove(old); err != nil {
			tlog("⚠️ [jwt-store] failed to prune old backup %s: %v\n", old, err)
		}
	}
}

// Get returns the stored entry for key, if any. It does not validate
// expiry/age — callers decide whether the entry is still usable.
func (s *clientJWTStore) Get(key string) (clientJWTEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	entry, ok := s.entries[key]
	return entry, ok
}

// Put records entry for key and flushes the store to disk immediately.
func (s *clientJWTStore) Put(key string, entry clientJWTEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.entries[key] = entry
	return s.flushLocked()
}

// Delete evicts key, if present, and flushes the store to disk immediately.
// Used when a reused client JWT turns out to be rejected server-side (e.g.
// the client_id was revoked) so the next mint attempt — this process's
// slow-retry loop, or the next restart — doesn't keep handing out the same
// poisoned identity.
func (s *clientJWTStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if _, ok := s.entries[key]; !ok {
		return nil
	}
	delete(s.entries, key)
	return s.flushLocked()
}

func (s *clientJWTStore) flushLocked() error {
	// In-memory-only mode (HOME unavailable at init): nothing to persist.
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
