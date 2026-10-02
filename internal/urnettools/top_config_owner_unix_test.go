//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

// Under sudo with HOME kept, a first save creates ~/.config/urnet-tools as root.
// The directory must be handed to the owner of the nearest directory that
// already existed, outermost first, or the user's next non-sudo save cannot
// write into it.
func TestMkdirAllHandOverChownsEveryNewDirToTheAncestorOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the temp dir is root-owned when the tests run as root, so there is nothing to hand over")
	}
	base := t.TempDir()
	fi, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)

	var calls []chownCall
	target := filepath.Join(base, ".config", "urnet-tools")
	err = mkdirAllHandOver(target, 0o755, true, func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	want := []chownCall{
		{filepath.Join(base, ".config"), int(st.Uid), int(st.Gid)},
		{target, int(st.Uid), int(st.Gid)},
	}
	if len(calls) != len(want) {
		t.Fatalf("chown calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("chown call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestMkdirAllHandOverLeavesExistingDirsAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("see above")
	}
	base := t.TempDir()
	var calls []chownCall
	if err := mkdirAllHandOver(base, 0o755, true, func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("chowned an existing directory: %+v", calls)
	}
}

func TestMkdirAllHandOverIsPlainMkdirAllWhenNotRoot(t *testing.T) {
	base := t.TempDir()
	called := false
	target := filepath.Join(base, "a", "b")
	if err := mkdirAllHandOver(target, 0o755, false, func(string, int, int) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("chown ran although the process is not root")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
}

// A chown failure must not fail the settings save.
func TestMkdirAllHandOverIgnoresChownFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("see above")
	}
	target := filepath.Join(t.TempDir(), "x", "y")
	err := mkdirAllHandOver(target, 0o755, true, func(string, int, int) error { return os.ErrPermission })
	if err != nil {
		t.Fatalf("a chown failure failed the mkdir: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
}
