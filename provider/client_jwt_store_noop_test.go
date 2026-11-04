package main

import (
	"os"
	"path/filepath"
	"testing"
)

func statOrFatal(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".client_jwts-*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Saving an entry that is already on disk exactly must not rewrite the file:
// a rewrite replaces the inode, so an untouched inode is the proof that no
// temp file was created, fsynced or renamed.
func TestJWTStoreUnchangedPutWritesNothing(t *testing.T) {
	s := newJWTStoreForTest(t)
	e := fixedEntry("jwt-a")
	if err := s.Put("p1", e); err != nil {
		t.Fatal(err)
	}
	before := statOrFatal(t, s.path)

	if err := s.Put("p1", e); err != nil {
		t.Fatal(err)
	}
	after := statOrFatal(t, s.path)

	if !os.SameFile(before, after) {
		t.Fatal("an unchanged Put replaced the store file")
	}
	if s.flushes != 1 || s.skipped != 1 {
		t.Fatalf("flushes=%d skipped=%d, want 1 and 1", s.flushes, s.skipped)
	}
	noTempLeft(t, filepath.Dir(s.path))
}

// Deleting a key another process already removed from disk is a no-op too.
func TestJWTStoreDeleteOfKeyAlreadyGoneWritesNothing(t *testing.T) {
	s := newJWTStoreForTest(t)
	if err := s.Put("p1", fixedEntry("jwt-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("p2", fixedEntry("jwt-b")); err != nil {
		t.Fatal(err)
	}
	// Another process drops p1 from disk; this process still holds it.
	other := newClientJWTStore(s.path)
	if err := other.Delete("p1"); err != nil {
		t.Fatal(err)
	}
	before := statOrFatal(t, s.path)

	if err := s.Delete("p1"); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, statOrFatal(t, s.path)) {
		t.Fatal("deleting a key that was already gone rewrote the file")
	}
}

// A save that changes content writes exactly once and is on disk, readable by
// a fresh process, before Put returns.
func TestJWTStoreChangedPutWritesOnceAndIsDurable(t *testing.T) {
	s := newJWTStoreForTest(t)
	if err := s.Put("p1", fixedEntry("jwt-a")); err != nil {
		t.Fatal(err)
	}
	before := statOrFatal(t, s.path)
	flushes := s.flushes

	if err := s.Put("p1", fixedEntry("jwt-b")); err != nil {
		t.Fatal(err)
	}

	if os.SameFile(before, statOrFatal(t, s.path)) {
		t.Fatal("a changed Put did not replace the store file")
	}
	if s.flushes != flushes+1 || s.skipped != 0 {
		t.Fatalf("flushes=%d (was %d) skipped=%d, want exactly one more write", s.flushes, flushes, s.skipped)
	}
	got, ok := newClientJWTStore(s.path).Get("p1")
	if !ok || got.ByClientJWT != "jwt-b" {
		t.Fatalf("a fresh reader sees %+v ok=%v, want jwt-b", got, ok)
	}
	noTempLeft(t, filepath.Dir(s.path))
}

// The skip compares against the file as read under the lock on every flush,
// not against a remembered digest, so a file changed underneath this process
// is never mistaken for "already saved".
func TestJWTStoreNoopSkipDoesNotFireWhenFileChangedUnderneath(t *testing.T) {
	s := newJWTStoreForTest(t)
	e := fixedEntry("jwt-a")
	if err := s.Put("p1", e); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){
		"emptied":   func() { os.WriteFile(s.path, []byte("{}"), 0600) },
		"corrupted": func() { os.WriteFile(s.path, []byte("not json"), 0600) },
		"removed":   func() { os.Remove(s.path) },
		"other key": func() { newClientJWTStore(s.path).Put("p2", fixedEntry("jwt-z")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.Put("p1", e); err != nil { // restore known state
				t.Fatal(err)
			}
			skipped := s.skipped
			mutate()
			if name == "other key" {
				// Disk now has p1 and p2; saving p1 again changes nothing and
				// must skip, the control for the cases above.
				if err := s.Put("p1", e); err != nil {
					t.Fatal(err)
				}
				if s.skipped != skipped+1 {
					t.Fatal("control: identical content should have skipped")
				}
				return
			}
			if err := s.Put("p1", e); err != nil {
				t.Fatal(err)
			}
			if s.skipped != skipped {
				t.Fatal("skip fired although the file was changed underneath")
			}
			got, ok := newClientJWTStore(s.path).Get("p1")
			if !ok || got.ByClientJWT != "jwt-a" {
				t.Fatalf("p1 not restored on disk: %+v ok=%v", got, ok)
			}
		})
	}
}

// A failed write still reaches the caller. The renewal path keeps its old
// token armed on this error, so the skip must never turn it into success.
func TestJWTStoreChangedPutStillReportsWriteFailure(t *testing.T) {
	s := newJWTStoreForTest(t)
	if err := s.Put("p1", fixedEntry("jwt-a")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := s.Put("p1", fixedEntry("jwt-b")); err == nil {
		t.Fatal("a changed Put into an unwritable directory returned nil")
	}
}
