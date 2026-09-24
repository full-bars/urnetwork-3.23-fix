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

	"github.com/urnetwork/connect"
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
	// flushes counts durable rewrites of the store file. Each one reads,
	// re-encodes and fsyncs the whole file, so callers that change many
	// entries at once must batch them into a single flush.
	flushes int
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

	ts := time.Now().UTC().Format("20060102T150405.000000Z")
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
	return s.flushLocked(key, entry, false)
}

// AnyNetworkID scans loaded store entries and returns the unambiguous non-empty
// NetworkID found across all entries. If entries contain conflicting non-empty
// NetworkIDs, it returns "" to prevent selecting an arbitrary network identity.
func (s *clientJWTStore) AnyNetworkID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	var unambiguous string
	for _, entry := range s.entries {
		if entry.NetworkID != "" {
			if unambiguous == "" {
				unambiguous = entry.NetworkID
			} else if unambiguous != entry.NetworkID {
				// Conflicting network IDs present in store — ambiguous, return empty.
				return ""
			}
		}
	}
	return unambiguous
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
	return s.flushLocked(key, clientJWTEntry{}, true)
}

// flushLocked persists the single Put/Delete operation named by key
// (entry/deleted) to disk. It reloads the durable store under the
// inter-process lock and merges this operation into that reloaded state
// before writing, rather than blindly marshaling s.entries (this process's
// in-memory snapshot, potentially loaded before another process — e.g. a
// HotSwap parent/candidate pair, each with its own clientJWTStore instance —
// flushed a different key). Without the reload, the second process to reach
// the lock would replace the first's update with its own stale map instead
// of merging: both Put calls would report success, but only the last
// process's snapshot survives on disk (F-9).
func (s *clientJWTStore) flushLocked(key string, entry clientJWTEntry, deleted bool) error {
	if deleted {
		return s.flushBatchLocked(nil, []string{key})
	}
	return s.flushBatchLocked(map[string]clientJWTEntry{key: entry}, nil)
}

// flushBatchLocked applies a set of upserts and deletes to the durable store
// with ONE read-merge-write-fsync cycle. The cost of a flush scales with the
// whole store, not with the change, so changing N entries must cost one flush,
// never N: a node with thousands of saved logins spent minutes at startup
// rewriting a multi-megabyte file once per adopted entry.
func (s *clientJWTStore) flushBatchLocked(upserts map[string]clientJWTEntry, deletes []string) error {
	// In-memory-only mode (HOME unavailable at init): nothing to persist.
	if s.path == "" {
		return nil
	}
	s.flushes++
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// Acquire inter-process lock so a concurrent HotSwap candidate cannot
	// overwrite our flush with its stale in-memory map (F-9).
	release, err := acquireJWTStoreLock(s.path)
	if err != nil {
		return fmt.Errorf("acquire jwt store lock: %w", err)
	}
	defer release()

	// Reload the durable store now, under the lock, so any entries a
	// concurrent process wrote since we last loaded are preserved.
	merged := s.entries
	if data, err := os.ReadFile(s.path); err == nil && len(data) > 0 {
		var onDisk map[string]clientJWTEntry
		if jsonErr := json.Unmarshal(data, &onDisk); jsonErr == nil {
			merged = onDisk
		}
		// A corrupt on-disk file falls back to this process's in-memory
		// entries (merged stays s.entries) rather than failing the flush —
		// loadLocked already snapshotted the corrupt bytes for recovery.
	}
	for _, key := range deletes {
		delete(merged, key)
	}
	for key, entry := range upserts {
		merged[key] = entry
	}
	s.entries = merged

	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}

	// Use unpredictable temp file to prevent collisions with concurrent writers (F-9)
	tmpFile, err := os.CreateTemp(dir, ".client_jwts-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, s.path)
}

// jwtStoreKey is the client JWT store key for a proxy: its identity
// (ProxySettings.Key(): the address, or address+user for a credentialed proxy),
// or the "direct" sentinel when there is no proxy. Keying by bare address made
// two accounts at one gateway share a single saved login, so one account's
// revocation eviction deleted the other's and both read as warm from one slot.
func jwtStoreKey(s *connect.ProxySettings) string {
	if s == nil {
		return "direct"
	}
	return s.Key()
}

// AdoptLegacy moves login entries stored under the legacy bare-address key to
// the identity keys of the proxies now at that address. Same rule as the other
// identity stores: when several accounts share an address, exactly one (the
// lexicographically smallest key) inherits the legacy login and the rest mint
// fresh, so no account is handed an identity it did not earn. The legacy slot is
// then removed even if the winner already had a newer entry, so a login that was
// later evicted as revoked can never be resurrected from it. An unauthenticated
// proxy at the address legitimately owns the bare slot, so that address is left
// alone. Idempotent. adopted counts logins moved; split counts accounts that had
// to mint fresh because the address was shared.
func (s *clientJWTStore) AdoptLegacy(desired []*connect.ProxySettings) (adopted, split int) {
	keysByAddress := map[string][]string{}
	for _, d := range desired {
		keysByAddress[d.Address] = append(keysByAddress[d.Address], d.Key())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	addresses := make([]string, 0, len(keysByAddress))
	for address := range keysByAddress {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	upserts := map[string]clientJWTEntry{}
	var deletes []string
	for _, address := range addresses {
		keys := keysByAddress[address]
		legacy, ok := s.entries[address]
		if !ok {
			continue
		}
		ownedByUnauthenticated := false
		for _, k := range keys {
			if k == address {
				ownedByUnauthenticated = true
			}
		}
		if ownedByUnauthenticated {
			continue
		}
		sort.Strings(keys)
		winner := keys[0]
		if _, has := s.entries[winner]; !has {
			s.entries[winner] = legacy
			upserts[winner] = legacy
			adopted++
			split += len(keys) - 1
		}
		delete(s.entries, address)
		deletes = append(deletes, address)
	}
	// One flush for the whole adoption, however many logins moved.
	if len(upserts) > 0 || len(deletes) > 0 {
		if err := s.flushBatchLocked(upserts, deletes); err != nil {
			tlog("⚠️ [jwt-store] failed to persist %d adopted logins (%d legacy slots dropped): %v\n", len(upserts), len(deletes), err)
		}
	}
	return adopted, split
}
