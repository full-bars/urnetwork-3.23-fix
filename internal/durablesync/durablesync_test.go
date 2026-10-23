package durablesync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write that renames into place is only durable once the directory entry is
// ordered, and that ordering must not fail where the platform has no directory
// flush. Every platform the miner ships must complete this call.
func TestSyncDirOrdersARealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal("sync directory:", err)
	}
}

// SyncParent orders the directory that names a path, which is the shape a
// caller holding only a pathname has.
func TestSyncParentOrdersTheRetainingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncParent(path); err != nil {
		t.Fatal("sync parent:", err)
	}
}

// A caller that keeps its directory descriptor for later use must not lose it
// to the ordering call.
func TestSyncRetainsABorrowedDescriptor(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := Sync(dir); err != nil {
		t.Fatal("sync retained descriptor:", err)
	}
	if _, err := dir.Stat(); err != nil {
		t.Fatal("Sync closed a descriptor it does not own:", err)
	}
}

// An absent directory is a real failure. Absorbing it would publish a
// credential whose rename nobody ordered.
func TestSyncDirReportsAnAbsentDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if err := SyncDir(missing); err == nil {
		t.Fatal("absent directory was reported as durable")
	}
	if err := SyncParent(filepath.Join(missing, "jwt")); err == nil {
		t.Fatal("absent parent directory was reported as durable")
	}
}

// A regular file is refused on every platform. This is the cross-platform
// guard for the shared precondition: on Unix a file flush succeeds, so without
// the precondition the two platforms would disagree about whether a completed
// call means anything, and this test would fail wherever it was run.
func TestSyncRefusesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	err = Sync(file)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a regular file must be refused with ErrNotDirectory, got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the refusal must name the descriptor it rejected, got %v", err)
	}
}

// SyncDir routes through the same precondition, so a mistaken pathname is
// refused rather than silently synced on the platforms where that succeeds.
func TestSyncDirRefusesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(path); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a regular file must be refused, got %v", err)
	}
	// The containing directory is still a directory, so the neighbour of that
	// refusal keeps working.
	if err := SyncParent(path); err != nil {
		t.Fatalf("SyncParent must order the containing directory, got %v", err)
	}
}

// A closed descriptor reports the descriptor, not the policy. This is the
// error a caller sees when a handle was released too early.
func TestSyncReportsAClosedDescriptor(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	err = Sync(directory)
	if err == nil {
		t.Fatal("a closed descriptor was reported as an ordered directory")
	}
	if errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a closed descriptor is not a directory-kind refusal: %v", err)
	}
}
