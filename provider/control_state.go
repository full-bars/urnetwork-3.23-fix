package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

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
}

// controlKeys are the only settings the socket accepts. Mirrors the 9
// existing ~/.urnetwork/* override files (node_name, report_url,
// report_interval, fast_auth, proxy_self_heal, proxy_url_max,
// proxy_url_refresh, proxy_dead_cleanup_scope, proxy_dead_cleanup_interval).
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
}

// globalControlState is the single provider-wide instance. Set by
// loadControlState (or newControlState if the persisted file is absent)
// during provider startup, before the control socket or any resolve*
// function can be reached.
var globalControlState = newControlState()

func newControlState() *controlState {
	return &controlState{values: map[string]string{}}
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

// set validates key against controlKeys and stores value. It does not
// persist to disk — callers that want durability call persist() after a
// successful set (see control_socket.go's command handler).
func (s *controlState) set(key, value string) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

// clear validates key against controlKeys and removes it, so a later get
// reports found=false (falls through to the legacy file / startup default)
// exactly as if it had never been set.
func (s *controlState) clear(key string) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

// snapshot returns a copy of every currently-set key, for persistence.
func (s *controlState) snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
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
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("provider_state.json: %w", err)
	}
	for k, v := range values {
		if controlKeys[k] {
			s.values[k] = v
		}
		// Silently drop any key this binary no longer recognizes rather
		// than failing startup over it — the provider is the only writer,
		// so an unknown key here means a newer version wrote it and this
		// binary was rolled back, not corruption.
	}
	return s, nil
}

// persist atomically writes the current snapshot to controlStatePath():
// temp file in the same directory, then os.Rename, so a crash mid-write
// never leaves a torn file. No flock is needed — the provider is the only
// process that ever writes this file.
func (s *controlState) persist() error {
	path, err := controlStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s.snapshot(), "", "  ")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
