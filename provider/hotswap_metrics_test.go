//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Both tests run against a temporary HOME. They used to write the real
// ~/.urnetwork/.hotswap_declines.json and restore it afterward, which could
// discard counters a provider on the same machine wrote in between.

func TestReadHotswapDeclinesFromDisk(t *testing.T) {
	withTempHome(t)
	stateDir := mustStateDir()
	if stateDir == "" {
		t.Skip("mustStateDir returned empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a decline file the same way urnet-tools CLI would.
	counts := map[string]int64{"version_old": 3, "unit_not_notify": 1, "success": 5}
	data, _ := json.Marshal(struct {
		Counts map[string]int64 `json:"counts"`
	}{Counts: counts})
	if err := os.WriteFile(filepath.Join(stateDir, ".hotswap_declines.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	result := readHotswapDeclinesFromDisk()
	if result == nil {
		t.Fatal("readHotswapDeclinesFromDisk returned nil for existing file")
	}
	if result["version_old"] != 3 {
		t.Errorf("version_old = %d, want 3", result["version_old"])
	}
	if result["unit_not_notify"] != 1 {
		t.Errorf("unit_not_notify = %d, want 1", result["unit_not_notify"])
	}
	if result["success"] != 5 {
		t.Errorf("success = %d, want 5", result["success"])
	}
}

func TestReadHotswapDeclinesFromDiskMissing(t *testing.T) {
	withTempHome(t)
	if mustStateDir() == "" {
		t.Skip("mustStateDir returned empty")
	}

	if result := readHotswapDeclinesFromDisk(); result != nil {
		t.Errorf("readHotswapDeclinesFromDisk with missing file returned %v, want nil", result)
	}
}
