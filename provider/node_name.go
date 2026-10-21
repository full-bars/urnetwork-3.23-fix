package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// nodeNameOverridePath returns ~/.urnetwork/node_name, a file an operator can
// write at any time to change the node identity reported to the fleet without
// restarting. An empty file or missing file falls back to the startup hostname.
func nodeNameOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "node_name"), nil
}

// resolveNodeName checks the control-socket state first, then the legacy
// override file, then startupName (the hostname captured at process start).
// Re-resolved on every call so a change takes effect on the reporter's next
// tick.
func resolveNodeName(startupName string) string {
	if v, ok := globalControlState.get("node_name"); ok && v != "" {
		return v
	}

	path, err := nodeNameOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return startupName
}

func parseProxyIndex(key string) int {
	idx := strings.IndexByte(key, ']')
	if idx <= 0 {
		return -1
	}
	start := strings.IndexByte(key[:idx], '[')
	if start < 0 {
		return -1
	}
	n, err := strconv.Atoi(key[start+1 : idx])
	if err != nil {
		return -1
	}
	return n
}
