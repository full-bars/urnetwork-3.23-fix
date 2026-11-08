//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func openLogForTest(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// An untouched log keeps its descriptor: no reopen, no churn on every pass.
func TestReopenIfOrphanedKeepsAMatchingDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urnetwork.log")
	f := openLogForTest(t, path)

	next, swapped, err := reopenIfOrphaned(path, f, 0644)
	if err != nil || swapped || next != nil {
		t.Fatalf("matching descriptor: next=%v swapped=%v err=%v", next, swapped, err)
	}
}

// The incident: a second process renames a trimmed file over the path. The
// first process's descriptor now names an unlinked inode; its writes must move
// to the file at the path.
func TestReopenIfOrphanedFollowsARenameOverThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork.log")
	stale := openLogForTest(t, path)
	stale.WriteString("before the trim\n")

	// What trimRAMLogByRename does in the other process.
	tmp := filepath.Join(dir, "urnetwork.log.trim.x")
	if err := os.WriteFile(tmp, []byte("kept tail\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	// Without the fix this write is lost: nothing at the path shows it.
	next, swapped, err := reopenIfOrphaned(path, stale, 0644)
	if err != nil || !swapped || next == nil {
		t.Fatalf("orphaned descriptor not detected: next=%v swapped=%v err=%v", next, swapped, err)
	}
	t.Cleanup(func() { next.Close() })
	if _, err := next.WriteString("after the trim\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "kept tail\nafter the trim\n" {
		t.Fatalf("log at the path = %q", got)
	}

	// A second pass on the new descriptor is quiet.
	if _, again, err := reopenIfOrphaned(path, next, 0644); err != nil || again {
		t.Fatalf("second pass swapped=%v err=%v", again, err)
	}
}

// An operator (or a tmpfs clean) removes the file: the log comes back instead
// of the process writing into the void.
func TestReopenIfOrphanedRecreatesARemovedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urnetwork.log")
	f := openLogForTest(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	next, swapped, err := reopenIfOrphaned(path, f, 0644)
	if err != nil || !swapped {
		t.Fatalf("removed log: swapped=%v err=%v", swapped, err)
	}
	t.Cleanup(func() { next.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log not recreated: %v", err)
	}
}

// A failed reopen reports the error and leaves the caller's descriptor alone.
func TestReopenIfOrphanedReportsAFailedReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork.log")
	f := openLogForTest(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	next, swapped, err := reopenIfOrphaned(path, f, 0644)
	if err == nil || swapped || next != nil {
		t.Fatalf("expected an error: next=%v swapped=%v err=%v", next, swapped, err)
	}
}
