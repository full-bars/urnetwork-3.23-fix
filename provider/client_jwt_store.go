package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// clientJWTStaleAfter prunes entries for proxies that haven't reconnected in
// this long — mostly relevant to URL-sourced proxies, whose addresses churn
// as dead entries get evicted and replaced.
const clientJWTStaleAfter = 30 * 24 * time.Hour

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
// commands in a bare environment (root, no HOME set — reproduced: the old
// init-time panic killed every invocation, shakedown M-section finding
// 2026-08-15). When HOME is unavailable, the store degrades to an
// in-memory-only store: identity reuse is lost for that process, but the
// command still runs. Same semantics as the load-error path below.
var globalClientJWTStore = func() *clientJWTStore {
	home, err := os.UserHomeDir()
	if err != nil {
		tlog("[jwt-store] HOME unavailable (%v) — in-memory only, no persistence\n", err)
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
		tlog("⚠️ [jwt-store] corrupt %s, starting fresh: %v\n", s.path, err)
		return
	}

	now := time.Now()
	pruned := make(map[string]clientJWTEntry, len(entries))
	for key, entry := range entries {
		if now.Sub(entry.MintedAt) < clientJWTStaleAfter {
			pruned[key] = entry
		}
	}
	s.entries = pruned
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
