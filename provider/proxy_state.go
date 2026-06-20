package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// proxyStateMu serializes all proxy.state read-modify-write cycles.
// Held during: heartbeat snapshot goroutine, reload() state write.
// Not needed for startup writes (heartbeat not yet running).
var proxyStateMu sync.Mutex

// ProxyState is the on-disk record of what the provider is currently running.
// Written atomically at startup and after each reload.
type ProxyState struct {
	Source    string                `json:"source"`     // live source file path ("" = internal config)
	StartedAt time.Time             `json:"started_at"` // provider process start time
	NextID    int                   `json:"next_id"`    // snapshot of counter for display
	Proxies   map[string]ProxyEntry `json:"proxies"`    // address -> entry
}

// ProxyEntry records the stable ID and last-known health for one proxy.
type ProxyEntry struct {
	ID        int    `json:"id"`
	Health    string `json:"health"`               // "up", "dead", "recently_offline", "offline", "long_offline", "inactive"
	DownSince string `json:"down_since,omitempty"` // RFC3339, set when not up
	Source    string `json:"source,omitempty"`     // "file", "internal", or "url" — where this address was first added from
}

func proxyStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.state"), nil
}

func readProxyState() (*ProxyState, error) {
	path, err := proxyStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyStateFrom(path)
}

func readProxyStateFrom(path string) (*ProxyState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyState{Proxies: map[string]ProxyEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy.state: %w", err)
	}
	var s ProxyState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse proxy.state: %w", err)
	}
	if s.Proxies == nil {
		s.Proxies = map[string]ProxyEntry{}
	}
	return &s, nil
}

func writeProxyState(s *ProxyState) error {
	path, err := proxyStatePath()
	if err != nil {
		return err
	}
	return writeProxyStateTo(path, s)
}

func writeProxyStateTo(path string, s *ProxyState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// resolveProxyID returns the stable ID for an address.
// Known addresses keep their existing ID; new ones get the next counter value.
func resolveProxyID(state *ProxyState, address string) int {
	if entry, ok := state.Proxies[address]; ok {
		return entry.ID
	}
	id := nextProxyID()
	state.Proxies[address] = ProxyEntry{ID: id}
	return id
}

// tagProxySourceIfUnset records where a proxy address was first added from
// ("file", "internal", or "url"). Once set, the tag is never overwritten —
// an address keeps its original provenance across reloads and restarts, so
// source-scoped dead-proxy cleanup stays accurate even if the same address
// later also appears in a different source.
func tagProxySourceIfUnset(state *ProxyState, address, source string) {
	entry := state.Proxies[address]
	if entry.Source == "" {
		entry.Source = source
	}
	state.Proxies[address] = entry
}
