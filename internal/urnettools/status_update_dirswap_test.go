package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The state dir can be replaced by a symlink while the release request runs.
// The cache write must not create or replace a file in the directory the
// symlink points at.
func TestStatusUpdateCacheWriteDoesNotFollowASwappedStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root := t.TempDir()
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")
	if err := os.Symlink(elsewhere, stateDir); err != nil {
		t.Fatal(err)
	}
	writeStatusUpdateCache(stateDir, statusUpdateState{At: time.Now(), Tag: "v1"})
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the write followed the swapped state dir and left %d file(s) behind", len(entries))
	}
}

func TestStatusUpdateCacheWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	writeStatusUpdateCache(dir, statusUpdateState{At: time.Now(), Tag: "v2"})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != statusUpdateCacheFile {
		t.Fatalf("expected only %s, got %v", statusUpdateCacheFile, entries)
	}
}
