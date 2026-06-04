package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
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
