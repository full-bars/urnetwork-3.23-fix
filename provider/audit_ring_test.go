package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuditRing_AppendAndEntries(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	// Append 5 entries
	for i := 0; i < 5; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			Value:     "nyc-1",
			OK:        true,
		})
	}

	// Get all 5, newest first
	entries, nextCursor := ring.Entries(10, "")
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}
	// Newest first: 10:00:04, 10:00:03, 10:00:02, 10:00:01, 10:00:00
	if entries[0].Timestamp.Second() != 4 {
		t.Errorf("first entry should be newest (sec=4), got %d", entries[0].Timestamp.Second())
	}
	if entries[4].Timestamp.Second() != 0 {
		t.Errorf("last entry should be oldest (sec=0), got %d", entries[4].Timestamp.Second())
	}
	if nextCursor != "" {
		t.Errorf("nextCursor should be empty when all returned, got %q", nextCursor)
	}
}

func TestAuditRing_Pagination(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	for i := 0; i < 10; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			OK:        true,
		})
	}

	// First page: 3 entries (newest first: sec=9,8,7)
	entries, nextCursor := ring.Entries(3, "")
	if len(entries) != 3 {
		t.Fatalf("page 1: expected 3, got %d", len(entries))
	}
	if nextCursor == "" {
		t.Fatal("page 1: nextCursor should not be empty")
	}
	if entries[0].Timestamp.Second() != 9 {
		t.Errorf("page 1 first entry sec=9, got %d", entries[0].Timestamp.Second())
	}

	// Second page: skip entries <= cursor (sec=6), remaining = [7,8,9]
	// Take last 2 (since only 2 remain): sec=9,8
	entries2, nextCursor2 := ring.Entries(3, nextCursor)
	if len(entries2) != 2 {
		t.Fatalf("page 2: expected 2, got %d", len(entries2))
	}
	if entries2[0].Timestamp.Second() != 9 {
		t.Errorf("page 2 first entry sec=9, got %d", entries2[0].Timestamp.Second())
	}
	// No more pages
	if nextCursor2 != "" {
		t.Errorf("page 2: nextCursor should be empty, got %q", nextCursor2)
	}
}

func TestAuditRing_WrapsAround(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	// Fill the ring past capacity (1000)
	for i := 0; i < 1050; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, i/60, i%60, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			OK:        true,
		})
	}

	// Should have exactly 1000 entries
	entries, _ := ring.Entries(2000, "")
	if len(entries) != 1000 {
		t.Fatalf("expected 1000 entries, got %d", len(entries))
	}

	// Newest should be entry 1049
	if entries[0].Timestamp.Minute() != 1049/60 || entries[0].Timestamp.Second() != 1049%60 {
		t.Errorf("newest entry unexpected: %v", entries[0].Timestamp)
	}

	// Oldest should be entry 50 (first 50 were overwritten)
	if entries[999].Timestamp.Minute() != 50/60 || entries[999].Timestamp.Second() != 50%60 {
		t.Errorf("oldest entry unexpected: %v", entries[999].Timestamp)
	}
}

func TestAuditRing_Empty(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	entries, nextCursor := ring.Entries(10, "")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("nextCursor should be empty, got %q", nextCursor)
	}
}

func TestAuditRing_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")

	// Create and persist
	ring := &AuditRing{path: path}
	for i := 0; i < 3; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			Value:     "nyc-1",
			OK:        true,
		})
	}
	if err := ring.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Load into new ring via loadJSONWithRecovery
	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	ok, err := loadJSONWithRecovery(path, &saved)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(saved.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(saved.Entries))
	}

	// Replay into new ring
	ring2 := &AuditRing{path: path}
	for _, e := range saved.Entries {
		ring2.Append(e)
	}

	entries, _ := ring2.Entries(10, "")
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries from ring2, got %d", len(entries))
	}
	// Verify content survived round-trip
	if entries[0].Key != "node_name" || entries[0].Value != "nyc-1" {
		t.Errorf("entry content wrong: %+v", entries[0])
	}
}

func TestAuditRing_NilSafe(t *testing.T) {
	// nil ring should not panic
	var ring *AuditRing
	ring.Append(CommandAudit{}) // should not panic
	entries, cursor := ring.Entries(10, "")
	if entries != nil || cursor != "" {
		t.Error("nil ring should return empty")
	}
}
