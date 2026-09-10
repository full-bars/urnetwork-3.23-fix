package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := map[string]uint64{"dns_timeout": 5, "conn_refused": 12}
	if err := atomicWriteJSON(path, data); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	// File should exist
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Parse envelope
	var envelope struct {
		Data     map[string]uint64 `json:"data"`
		Checksum string            `json:"checksum"`
		SavedAt  string            `json:"saved_at"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	// Verify checksum
	expectedChecksum := fmt.Sprintf("sha256:%x", sha256.Sum256(
		func() []byte { b, _ := json.Marshal(data); return b }(),
	))
	if envelope.Checksum != expectedChecksum {
		t.Errorf("checksum mismatch: got %s, want %s", envelope.Checksum, expectedChecksum)
	}

	// Verify data
	if envelope.Data["dns_timeout"] != 5 || envelope.Data["conn_refused"] != 12 {
		t.Errorf("data mismatch: %+v", envelope.Data)
	}

	// .bak should NOT exist yet (first write)
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error(".bak should not exist after first write")
	}
}

func TestAtomicWriteJSON_CreatesBakOnSecondWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	// First write
	atomicWriteJSON(path, map[string]uint64{"a": 1})

	// Second write — should create .bak
	if err := atomicWriteJSON(path, map[string]uint64{"a": 2}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Error(".bak should exist after second write")
	}

	// Primary should have new value
	var envelope struct {
		Data map[string]uint64 `json:"data"`
	}
	raw, _ := os.ReadFile(path)
	json.Unmarshal(raw, &envelope)
	if envelope.Data["a"] != 2 {
		t.Errorf("primary should have a=2, got %+v", envelope.Data)
	}

	// Backup should have old value
	rawBak, _ := os.ReadFile(path + ".bak")
	json.Unmarshal(rawBak, &envelope)
	if envelope.Data["a"] != 1 {
		t.Errorf("backup should have a=1, got %+v", envelope.Data)
	}
}

func TestLoadJSONWithRecovery_PrimaryValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	atomicWriteJSON(path, map[string]string{"key": "value"})

	var result map[string]string
	ok, err := loadJSONWithRecovery(path, &result)
	if err != nil {
		t.Fatalf("loadJSONWithRecovery: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if result["key"] != "value" {
		t.Errorf("got %+v, want key=value", result)
	}
}

func TestLoadJSONWithRecovery_PrimaryCorrupt_BackupValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	// Write good file
	atomicWriteJSON(path, map[string]uint64{"x": 1})
	// Second write creates .bak from the first write
	atomicWriteJSON(path, map[string]uint64{"x": 42})

	// Corrupt the primary
	os.WriteFile(path, []byte("not json at all"), 0600)

	var result map[string]uint64
	ok, err := loadJSONWithRecovery(path, &result)
	if err != nil {
		t.Fatalf("loadJSONWithRecovery: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true from backup")
	}
	if result["x"] != 1 {
		t.Errorf("got %+v, want x=1 (from backup)", result)
	}

	// Primary should have been restored from backup
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "not json") {
		t.Error("primary should have been restored from backup")
	}
}

func TestLoadJSONWithRecovery_BothCorrupt_Quarantines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	// Write good file, then corrupt both primary and backup
	atomicWriteJSON(path, map[string]uint64{"x": 1})
	os.WriteFile(path, []byte("corrupt primary"), 0600)
	os.WriteFile(path+".bak", []byte("corrupt backup"), 0600)

	var result map[string]uint64
	ok, err := loadJSONWithRecovery(path, &result)
	if err != nil {
		t.Fatalf("loadJSONWithRecovery: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when both files corrupt")
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %+v", result)
	}

	// Should have quarantined the corrupt primary
	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) == 0 {
		t.Error("expected quarantine file")
	}
}

func TestLoadJSONWithRecovery_NoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	var result map[string]uint64
	ok, err := loadJSONWithRecovery(path, &result)
	if err != nil {
		t.Fatalf("loadJSONWithRecovery: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for nonexistent file")
	}
}

func TestLoadJSONWithRecovery_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	// Manually write a file with wrong checksum
	envelope := map[string]interface{}{
		"data":     map[string]uint64{"x": 1},
		"checksum": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"saved_at": "2026-01-01T00:00:00Z",
	}
	blob, _ := json.MarshalIndent(envelope, "", "  ")
	os.WriteFile(path, blob, 0600)

	var result map[string]uint64
	ok, err := loadJSONWithRecovery(path, &result)
	if err != nil {
		t.Fatalf("loadJSONWithRecovery: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for checksum mismatch")
	}
}

func TestPeriodicFlush_Integration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error_counts.json")

	// Create a persistentErrors with a low dirty threshold for testing
	pe := &persistentErrors{
		counts: map[string]uint64{},
		path:   path,
		dirty:  0,
	}

	// Simulate some errors
	pe.mu.Lock()
	pe.counts["dns_timeout"] = 5
	pe.counts["conn_refused"] = 12
	pe.dirty = 101 // above threshold
	pe.mu.Unlock()

	// Flush manually
	pe.flushAtomic()

	// Verify file written with envelope
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var envelope struct {
		Data     map[string]uint64 `json:"data"`
		Checksum string            `json:"checksum"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Data["dns_timeout"] != 5 {
		t.Errorf("dns_timeout: got %d, want 5", envelope.Data["dns_timeout"])
	}
	if !strings.HasPrefix(envelope.Checksum, "sha256:") {
		t.Errorf("checksum format: %s", envelope.Checksum)
	}

	// Dirty counter should be reset
	pe.mu.Lock()
	if pe.dirty != 0 {
		t.Errorf("dirty should be 0 after flush, got %d", pe.dirty)
	}
	pe.mu.Unlock()
}

func TestFlushAtomic_SkipsWhenNotDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error_counts.json")

	pe := &persistentErrors{
		counts: map[string]uint64{"x": 1},
		path:   path,
		dirty:  0,
	}

	// Should not create file when dirty=0
	pe.flushAtomic()
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not exist when dirty=0")
	}
}
