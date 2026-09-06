package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writePendingOverrides(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "pending_overrides.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestMergePendingOverrides_MissingFileIsNoop(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	mergePendingOverrides(s) // must not panic or error on a fresh box

	if _, found := s.get("node_name"); found {
		t.Fatalf("expected empty state, nothing was queued")
	}
}

func TestMergePendingOverrides_AppliesInOrderThenDeletesFile(t *testing.T) {
	home := withTempHome(t)
	path := writePendingOverrides(t, home, `[
		{"op":"set","key":"node_name","value":"nyc-1"},
		{"op":"set","key":"node_name","value":"nyc-2"},
		{"op":"set","key":"fast_auth","value":"on"},
		{"op":"clear","key":"fast_auth"}
	]`)
	s := newControlState()

	mergePendingOverrides(s)

	if v, found := s.get("node_name"); !found || v != "nyc-2" {
		t.Fatalf("node_name = (%q, %v), want (%q, true) — ops must apply in order", v, found, "nyc-2")
	}
	if _, found := s.get("fast_auth"); found {
		t.Fatalf("fast_auth should have been cleared by the last op")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pending_overrides.json should be deleted after a successful merge, stat err = %v", err)
	}

	// The merge must have persisted, not just changed memory.
	reloaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	if v, found := reloaded.get("node_name"); !found || v != "nyc-2" {
		t.Fatalf("persisted node_name = (%q, %v), want (%q, true)", v, found, "nyc-2")
	}
}

func TestMergePendingOverrides_MalformedJSONLeftInPlace(t *testing.T) {
	home := withTempHome(t)
	path := writePendingOverrides(t, home, `not json`)
	s := newControlState()

	mergePendingOverrides(s) // must not crash startup over a bad queue file

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("malformed pending_overrides.json should be left for inspection, got stat err = %v", err)
	}
}

func TestMergePendingOverrides_InvalidEntrySkippedOthersApplied(t *testing.T) {
	home := withTempHome(t)
	path := writePendingOverrides(t, home, `[
		{"op":"set","key":"not-a-real-key","value":"x"},
		{"op":"bogus-op","key":"node_name","value":"x"},
		{"op":"set","key":"node_name","value":"nyc-1"}
	]`)
	s := newControlState()

	mergePendingOverrides(s)

	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name = (%q, %v), want (%q, true) — the one valid op should still apply", v, found, "nyc-1")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("queue file should still be deleted once the valid entries are applied, stat err = %v", err)
	}
}

// TestMergePendingOverrides_PersistFailureLeavesFileForRetry covers the
// case where the in-memory apply succeeds but the disk write doesn't — the
// queue file must survive so the next startup retries instead of silently
// losing the queued change.
func TestMergePendingOverrides_PersistFailureLeavesFileForRetry(t *testing.T) {
	home := withTempHome(t)
	path := writePendingOverrides(t, home, `[{"op":"set","key":"node_name","value":"nyc-1"}]`)
	s := newControlState()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	mergePendingOverrides(s)

	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("in-memory apply should still have happened even though persist failed, got (%q, %v)", v, found)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("queue file should be kept for retry when persist fails, stat err = %v", err)
	}
}
