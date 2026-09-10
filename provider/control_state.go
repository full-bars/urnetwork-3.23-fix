package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Source identifies where a control setting value originated.
type Source string

const (
	SourceSocket  Source = "socket"
	SourceEnv     Source = "env"
	SourcePending Source = "pending"
	SourceLegacy  Source = "legacy"
	SourceDefault Source = "default"
)

// configMeta records the provenance of a single control setting.
type configMeta struct {
	Source Source    `json:"source"`
	SetAt  time.Time `json:"set_at,omitempty"`
}

// controlStateEnvelope is the v2 on-disk format for provider_state.json.
// It wraps the values map with a version tag and per-key metadata.
type controlStateEnvelope struct {
	Version int                   `json:"version"`
	Values  map[string]string     `json:"values"`
	Meta    map[string]configMeta `json:"meta,omitempty"`
}

// controlState is the provider's own in-memory record of every runtime
// setting an operator can change via the control socket (control_socket.go).
// It is the ONLY thing that writes controlStatePath() — unlike the legacy
// per-file overrides (still read as a fallback below, one function per
// setting), there is exactly one writer here, so none of the read-modify-
// write or stale-cache problems that came with a shared, externally-written
// file apply. All access goes through this type's methods, which hold mu for
// their whole body.
type controlState struct {
	mu     sync.RWMutex
	values map[string]string
	meta   map[string]configMeta
	// passthrough preserves unrecognized keys from v2 envelopes across
	// load -> persist round-trips so a rollback doesn't drop them.
	passthroughValues map[string]string
	passthroughMeta   map[string]configMeta
	// txMu serializes the read-modify-write-persist-rollback sequence that
	// makes up one logical `set`/`clear` operation. Each control-socket
	// connection is served on its own goroutine (handleControlConn), and the
	// individual s.mu lock in set/clear/get/persist does NOT span that whole
	// sequence — without txMu, two concurrent sets on the same key could
	// interleave (G1 reads old, G2 reads old, G1 writes A, G2 writes B, G1
	// persists, G2's persist-failure rolls back to its own stale view or
	// clears B). txMu makes the get-old → set/clear → persist → rollback unit
	// atomic for this state.
	txMu sync.Mutex
}

// controlKeys are the only settings the socket accepts.
//
// The first 9 mirror the existing ~/.urnetwork/* override files
// (node_name, report_url, report_interval, fast_auth, proxy_self_heal,
// proxy_url_max, proxy_url_refresh, proxy_dead_cleanup_scope,
// proxy_dead_cleanup_interval).
//
// The remaining 5 mirror settings previously managed only via systemd
// override.conf (see scripts/Provider_Install_Linux.sh's
// override_set_env/override_rm_env), which required a full restart to
// take effect and were edited by sed-ing a hand-written drop-in file.
// Fully wired here (apply immediately, no restart) via
// handleSetSideEffects in control_socket.go: hot_restart, gomemlimit,
// gogc. profile and ramlogs are read via os.Getenv("URNETWORK_PROFILE"/
// "URNETWORK_RAMLOGS") in main()/initGlog()/RunStartupAudit(), all of
// which run before provide() (and therefore before loadControlState) even
// executes — initGlog in particular runs from this package's init(),
// before main() even starts, making its one-shot decision about
// redirecting stdout/stderr to a ramlog. loadControlState()/
// mergePendingOverrides() inside provide() can't reach back in time to
// affect that. See startup_env_seed.go: init() seeds those two env vars
// from provider_state.json + pending_overrides.json before initGlog()
// runs, so `set profile`/`set ramlogs` over the socket still requires a
// restart (inherent to what they configure — buffer/worker sizing baked
// into objects allocated once at startup, and a live stdout/stderr
// redirect respectively) but is correctly picked up on that restart,
// including the very first one after a fresh-install `urnet-tools set`
// queued while the provider wasn't running yet.
var controlKeys = map[string]bool{
	"node_name":                   true,
	"report_url":                  true,
	"report_interval":             true,
	"fast_auth":                   true,
	"proxy_self_heal":             true,
	"proxy_url_max":               true,
	"proxy_url_refresh":           true,
	"proxy_dead_cleanup_scope":    true,
	"proxy_dead_cleanup_interval": true,
	"hot_restart":                 true,
	"gomemlimit":                  true,
	"gogc":                        true,
	"profile":                     true,
	"ramlogs":                     true,
	"metrics":                     true,
}

// globalControlState is the single provider-wide instance. Set by
// loadControlState (or newControlState if the persisted file is absent)
// during provider startup, before the control socket or any resolve*
// function can be reached.
var globalControlState = newControlState()

func newControlState() *controlState {
	return &controlState{
		values:            map[string]string{},
		meta:              map[string]configMeta{},
		passthroughValues: map[string]string{},
		passthroughMeta:   map[string]configMeta{},
	}
}

// get returns a setting's raw string value and whether it has been set via
// the control socket. found=false means "the socket has no opinion on this
// key" — the caller should fall back to its legacy file / startup default,
// exactly as if the socket didn't exist.
func (s *controlState) get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// getWithMeta returns a setting's value, its config metadata, and whether
// it was found in the state. Unlike get(), callers always get the meta
// (zero-valued configMeta if not set via the socket).
func (s *controlState) getWithMeta(key string) (string, configMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, s.meta[key], ok
}

// set validates key against controlKeys and stores value with SourceSocket.
// It does not persist to disk — callers that want durability call
// persist() after a successful set (see control_socket.go's command handler).
func (s *controlState) set(key, value string) error {
	return s.setWithValue(key, value, SourceSocket)
}

// setWithValue validates key against controlKeys and stores value + meta
// under the write lock. Does not persist — callers must persist() after.
func (s *controlState) setWithValue(key, value string, src Source) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	s.meta[key] = configMeta{Source: src, SetAt: time.Now()}
	// Remove from passthrough if it was there — prevents stale passthrough
	// values from clobbering a live set at persist time.
	delete(s.passthroughValues, key)
	delete(s.passthroughMeta, key)
	return nil
}

// clear validates key against controlKeys and removes it, so a later get
// reports found=false (falls through to the legacy file / startup default)
// exactly as if it had never been set.
func (s *controlState) clear(key string) error {
	return s.clearWithValue(key)
}

// clearWithValue validates key, deletes value, meta, and passthrough entries
// under the write lock.
func (s *controlState) clearWithValue(key string) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	delete(s.meta, key)
	delete(s.passthroughValues, key)
	delete(s.passthroughMeta, key)
	return nil
}

// replaceAll swaps the entire value set under the same mutex every get/set
// caller already uses. Used to adopt a freshly reloaded provider_state.json
// (e.g. a HotSwap candidate reloading after the parent releases its socket)
// without ever reassigning the globalControlState *controlState pointer
// itself — every resolve*/*Enabled function and proxy goroutine holds that
// pointer for the process's entire lifetime, so swapping it out from under
// them would be an unsynchronized read/write race on the pointer variable,
// not just its pointed-to map (which s.mu alone would not protect against).
func (s *controlState) replaceAll(values map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = values
	// Clear meta for replaced values — callers using this legacy path
	// don't carry metadata.
	s.meta = make(map[string]configMeta, len(values))
	s.passthroughValues = map[string]string{}
	s.passthroughMeta = map[string]configMeta{}
}

// replaceAllWithMeta swaps both values and meta under the write lock.
// Used by the HotSwap path when a full v2 reload is available.
func (s *controlState) replaceAllWithMeta(values map[string]string, meta map[string]configMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = values
	s.meta = meta
	// Preserve passthrough maps — loadControlState already loaded them
	// from disk; resetting here would drop forward-compatible keys.
}

// snapshot returns a copy of every currently-set key, for backward
// compatibility with callers that only need map[string]string.
func (s *controlState) snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// snapshotV2 returns the full v2 envelope with values + meta, suitable for
// serializing to the v2 on-disk format.
func (s *controlState) snapshotV2() controlStateEnvelope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vals := make(map[string]string, len(s.values)+len(s.passthroughValues))
	for k, v := range s.values {
		vals[k] = v
	}
	for k, v := range s.passthroughValues {
		vals[k] = v
	}
	meta := make(map[string]configMeta, len(s.meta)+len(s.passthroughMeta))
	for k, m := range s.meta {
		meta[k] = m
	}
	for k, m := range s.passthroughMeta {
		meta[k] = m
	}
	return controlStateEnvelope{
		Version: 2,
		Values:  vals,
		Meta:    meta,
	}
}

// statusSnapshot returns every known setting with its value and metadata,
// intended for status / diagnostic endpoints. Keys not in s.values are
// resolved from environment or code defaults so the operator always sees
// the full picture — not just socket-overridden keys.
func (s *controlState) statusSnapshot() map[string]struct {
	Value string
	Meta  configMeta
} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Pre-populate with env or code defaults for all known keys.
	envDefaults := map[string]string{
		"ramlogs":      os.Getenv("URNETWORK_RAMLOGS"),
		"fast_auth":    os.Getenv("URNETWORK_FAST_AUTH"),
		"metrics":      os.Getenv("URNETWORK_METRICS"),
		"profile":      os.Getenv("URNETWORK_PROFILE"),
		"gogc":         os.Getenv("GOGC"),
		"gomemlimit":   os.Getenv("GOMEMLIMIT"),
	}

	out := make(map[string]struct {
		Value string
		Meta  configMeta
	}, len(controlKeys))
	for k := range controlKeys {
		if v, ok := s.values[k]; ok {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: v, Meta: s.meta[k]}
		} else if v := envDefaults[k]; v != "" {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: v, Meta: configMeta{Source: SourceEnv}}
		} else {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: "", Meta: configMeta{Source: SourceDefault}}
		}
	}
	return out
}

// controlStatePath returns ~/.urnetwork/provider_state.json — the provider's
// own private, atomically-written record of every socket-set control key.
// Unlike the legacy override files, nothing but the provider process itself
// ever reads or writes this file.
func controlStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "provider_state.json"), nil
}

// loadControlState reads controlStatePath() into a fresh controlState. A
// missing file is not an error — it means no setting has ever been changed
// via the socket, so every resolve* function falls all the way through to
// its legacy file / startup default, same as before this feature existed.
//
// It handles both v2 envelopes ({"version":2, "values":{...}, "meta":{...}})
// and legacy flat maps ({"key":"value", ...}) for backward compatibility.
func loadControlState() (*controlState, error) {
	path, err := controlStatePath()
	if err != nil {
		return nil, err
	}
	s := newControlState()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}

	// Try v2 envelope first (detected by "version" field).
	var envelope controlStateEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version > 0 {
		for k, v := range envelope.Values {
			if controlKeys[k] {
				s.values[k] = v
				if m, ok := envelope.Meta[k]; ok {
					s.meta[k] = m
				}
			} else {
				// Unrecognized keys in v2 are preserved in a passthrough
				// section so a rollback doesn't drop them.
				s.passthroughValues[k] = v
				if m, ok := envelope.Meta[k]; ok {
					s.passthroughMeta[k] = m
				}
			}
		}
		for k, m := range envelope.Meta {
			if !controlKeys[k] {
				s.passthroughMeta[k] = m
			}
		}
		return s, nil
	}

	// Legacy flat map (no version field).
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("provider_state.json: %w", err)
	}
	for k, v := range values {
		if controlKeys[k] {
			s.values[k] = v
			// Legacy-loaded values get SourceLegacy with zero time.
			s.meta[k] = configMeta{Source: SourceLegacy}
		}
		// Silently drop any key this binary no longer recognizes rather
		// than failing startup over it — the provider is the only writer,
		// so an unknown key here means a newer version wrote it and this
		// binary was rolled back, not corruption.
	}
	return s, nil
}

// persist atomically writes the current snapshot to controlStatePath() in
// v2 envelope format: temp file in the same directory, fsync, rename, then
// fsync the parent directory. No flock is needed — the provider is the only
// process that ever writes this file.
func (s *controlState) persist() error {
	path, err := controlStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	envelope := s.snapshotV2()
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".provider_state.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Best-effort directory fsync — don't fail after successful rename.
	// This breaks on Windows and can cause split-brain if it fails.
	if parent, err := os.Open(filepath.Dir(path)); err == nil {
		parent.Sync()
		parent.Close()
	}
	return nil
}
