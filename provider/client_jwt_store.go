package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// clientJWTMaxAge caps how long a stored client JWT is reused before this
// process mints a fresh one, kept comfortably under the platform's 30-day
// ClientExpiration so a stale-but-not-yet-rejected JWT is never handed to a
// caller close to its hard expiry.
const clientJWTMaxAge = 25 * 24 * time.Hour

// clientJWTStaleAfter prunes entries for proxies that haven't reconnected in
// this long — mostly relevant to URL-sourced proxies, whose addresses churn
// as dead entries get evicted and replaced.
const clientJWTStaleAfter = 30 * 24 * time.Hour

type clientJWTEntry struct {
	ByClientJWT string    `json:"by_client_jwt"`
	ClientID    string    `json:"client_id"`
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

var globalClientJWTStore = func() *clientJWTStore {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
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

func (s *clientJWTStore) flushLocked() error {
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
