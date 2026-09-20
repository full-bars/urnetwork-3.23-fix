//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func assertNothingWritten(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if entries, _ := os.ReadDir(d); len(entries) != 0 {
			t.Fatalf("%s was modified: %v", d, entries)
		}
	}
}

// A state dir that is itself a symlink is refused, and the link target is
// left untouched.
func TestWriteRestartMarkerRefusesSymlinkedStateDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := writeRestartMarkerIn("", link, "update", time.Now()); err == nil {
		t.Fatal("wrote through a symlinked state dir")
	}
	assertNothingWritten(t, real)
}

// A symlinked COMPONENT of the state dir path, beneath the trusted home root,
// is refused: the walk from root never follows one.
func TestWriteRestartMarkerRefusesSymlinkedComponent(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Mkdir(filepath.Join(elsewhere, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	// home/user -> elsewhere, so home/user/.urnetwork resolves outside home.
	if err := os.Symlink(elsewhere, filepath.Join(home, "user")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	stateDir := filepath.Join(home, "user", ".urnetwork")
	if err := writeRestartMarkerIn(home, stateDir, "update", time.Now()); err == nil {
		t.Fatal("wrote through a symlinked state-dir component")
	}
	assertNothingWritten(t, filepath.Join(elsewhere, ".urnetwork"))
	// The same failure through the best-effort wrapper is only a note.
	recordRestartReason(Provider{StateHome: home, StateDir: stateDir}, "manual")
	assertNothingWritten(t, filepath.Join(elsewhere, ".urnetwork"))
}

// A symlink planted at the marker name is not written through.
func TestWriteRestartMarkerRefusesPlantedSymlinkAtMarker(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, ".restart-reason")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := writeRestartMarkerIn("", dir, "manual", time.Now()); err == nil {
		t.Fatal("wrote through a symlink at the marker name")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
}

// Beneath a trusted root a plain state dir works.
func TestWriteRestartMarkerBeneathTrustedRoot(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".urnetwork")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRestartMarkerIn(home, stateDir, "hotswap", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(stateDir, ".restart-reason"))
	if err != nil || !markerLineRe.Match(b) {
		t.Fatalf("marker = %q, %v", b, err)
	}
}
