//go:build openbsd

package urnettools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpenBSDProviderCandidates(t *testing.T) {
	want := []string{openbsdLibexecProviderPath, "/home/test/.local/share/urnetwork-provider/bin/urnetwork", "/home/test/.urnetwork/urnetwork"}
	if got := openbsdProviderCandidates("/home/test"); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if got := openbsdProviderCandidates(""); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("candidates without home = %v", got)
	}
}

func TestOpenBSDInstalledProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root", "urnetwork")
	old := openbsdLibexecProviderPath
	openbsdLibexecProviderPath = root
	t.Cleanup(func() { openbsdLibexecProviderPath = old })
	home := t.TempDir()
	legacy := filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork")
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
	if got := installedOpenBSDProviderIn(home); got != "" {
		t.Fatalf("missing = %q", got)
	}
	write(root, 0700)
	write(legacy, 0755)
	if got := installedOpenBSDProviderIn(home); got != legacy {
		t.Fatalf("fallback = %q, want %q", got, legacy)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if got := installedOpenBSDProviderIn(home); got != root {
		t.Fatalf("preferred = %q, want %q", got, root)
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
