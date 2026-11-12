//go:build openbsd

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBSDInstalledProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root", "urnetwork")
	old := openbsdLibexecProviderPath
	openbsdLibexecProviderPath = root
	t.Cleanup(func() { openbsdLibexecProviderPath = old })
	oldUID := openbsdProviderOwnerUID
	t.Cleanup(func() { openbsdProviderOwnerUID = oldUID })
	write := func(path string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if got := installedOpenBSDProvider(); got != "" {
		t.Fatalf("missing = %q", got)
	}
	write(root, 0700)
	if got := installedOpenBSDProvider(); got != "" {
		t.Fatalf("not other-executable = %q", got)
	}
	write(root, 0775)
	if got := installedOpenBSDProvider(); got != "" {
		t.Fatalf("group-writable = %q", got)
	}
	write(root, 0755)
	// The test file is owned by the current user, so root ownership rejects it.
	openbsdProviderOwnerUID = uint32(os.Getuid()) + 1
	if got := installedOpenBSDProvider(); got != "" {
		t.Fatalf("non-root owner = %q", got)
	}
	openbsdProviderOwnerUID = uint32(os.Getuid())
	if got := installedOpenBSDProvider(); got != root {
		t.Fatalf("accepted = %q, want %q", got, root)
	}
}

func TestStateDirIsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if stateDirIsSymlink(real) || !stateDirIsSymlink(link) || stateDirIsSymlink("") || stateDirIsSymlink(filepath.Join(dir, "none")) {
		t.Fatal("unexpected symlink verdicts")
	}
}

func TestOpenBSDIgnoresHomeBinaries(t *testing.T) {
	old := openbsdLibexecProviderPath
	openbsdLibexecProviderPath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { openbsdLibexecProviderPath = old })
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, rel := range []string{".local/share/urnetwork-provider/bin/urnetwork", ".urnetwork/urnetwork"} {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if got := installedOpenBSDProvider(); got != "" {
		t.Fatalf("home binary accepted: %q", got)
	}
}

func TestOpenBSDDiscoverProcesses(t *testing.T) {
	dir := t.TempDir()
	ps := filepath.Join(dir, "ps")
	if err := os.WriteFile(ps, []byte("#!/bin/sh\nprintf '123 4294967295 urnetwork\\n456 0 unrelated\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := discoverProcesses()
	if len(got) != 1 || got[0].PID != 123 || !got[0].Running {
		t.Fatalf("providers = %+v, want one running provider with pid 123", got)
	}
	if got[0].User != "" || got[0].StateDir != "" || got[0].Binary != "" {
		t.Fatalf("unresolved owner must not acquire identity or binary: %+v", got[0])
	}
}
