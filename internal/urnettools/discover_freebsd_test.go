//go:build freebsd

package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The installer writes the provider to a root-owned tree that is not derived
// from a home directory. A candidate list built only from $HOME cannot find a
// stopped provider installed there, which is every install the current script
// makes.
func TestBSDProviderCandidatesPrefersTheRootOwnedTree(t *testing.T) {
	got := bsdProviderCandidates("/home/urnet")
	want := []string{
		"/usr/local/libexec/urnetwork-provider/bin/urnetwork",
		"/home/urnet/.local/share/urnetwork-provider/bin/urnetwork",
		"/home/urnet/.urnetwork/urnetwork",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBSDProviderCandidatesKeepTheRootTreeWithoutAHome(t *testing.T) {
	got := bsdProviderCandidates("")
	if len(got) != 1 || got[0] != "/usr/local/libexec/urnetwork-provider/bin/urnetwork" {
		t.Fatalf("candidates for an unknown home = %v, want only the root-owned tree", got)
	}
}

// useWritableBSDProviderRoot points the root-owned candidate at a writable tree
// with the same shape and restores the real path afterwards. The installer's
// tree is owned by root, so its file modes cannot be exercised as an
// unprivileged test user.
func useWritableBSDProviderRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "urnetwork-provider", "bin", "urnetwork")
	previous := bsdLibexecProviderPath
	bsdLibexecProviderPath = root
	t.Cleanup(func() { bsdLibexecProviderPath = previous })
	return root
}

func writeBSDProviderFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile's mode is masked by the umask, so set it explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// A stale or half-written file in the root-owned tree exists but cannot run.
// Accepting it hides a working per-user install that comes later in the list.
func TestFreeBSDInstalledProviderSkipsNonExecutableLibexec(t *testing.T) {
	root := useWritableBSDProviderRoot(t)
	writeBSDProviderFile(t, root, 0o644)
	home := t.TempDir()
	legacy := filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork")
	writeBSDProviderFile(t, legacy, 0o755)

	if got := installedBSDProviderIn(home); got != legacy {
		t.Fatalf("installed provider = %q, want the legacy per-user binary %q", got, legacy)
	}
}

// A root-owned candidate must be executable by the unprivileged service user.
func TestFreeBSDInstalledProviderSkipsLibexecWithoutOtherExecute(t *testing.T) {
	root := useWritableBSDProviderRoot(t)
	writeBSDProviderFile(t, root, 0o700)
	home := t.TempDir()
	legacy := filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork")
	writeBSDProviderFile(t, legacy, 0o700)

	if got := installedBSDProviderIn(home); got != legacy {
		t.Fatalf("installed provider = %q, want the per-user binary %q", got, legacy)
	}
}

// A per-user binary whose owner-execute bit is clear cannot be run by its
// owner even when group or other can, so it must not hide the working copy
// further down the list.
func TestFreeBSDInstalledProviderSkipsPerUserWithoutOwnerExecute(t *testing.T) {
	useWritableBSDProviderRoot(t)
	home := t.TempDir()
	writeBSDProviderFile(t, filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork"), 0o654)
	fallback := filepath.Join(home, ".urnetwork", "urnetwork")
	writeBSDProviderFile(t, fallback, 0o755)

	if got := installedBSDProviderIn(home); got != fallback {
		t.Fatalf("installed provider = %q, want the executable binary %q", got, fallback)
	}
}

// A directory satisfies os.Stat as well, and running it is impossible.
func TestFreeBSDInstalledProviderSkipsDirectoryAtLibexec(t *testing.T) {
	root := useWritableBSDProviderRoot(t)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	home := t.TempDir()
	legacy := filepath.Join(home, ".urnetwork", "urnetwork")
	writeBSDProviderFile(t, legacy, 0o755)

	if got := installedBSDProviderIn(home); got != legacy {
		t.Fatalf("installed provider = %q, want the legacy per-user binary %q", got, legacy)
	}
}

// Tightening the check must not demote a healthy root-owned install, which is
// the location the current installer writes and keeps its priority.
func TestFreeBSDInstalledProviderPrefersExecutableLibexec(t *testing.T) {
	root := useWritableBSDProviderRoot(t)
	writeBSDProviderFile(t, root, 0o755)
	home := t.TempDir()
	writeBSDProviderFile(t, filepath.Join(home, ".urnetwork", "urnetwork"), 0o755)

	if got := installedBSDProviderIn(home); got != root {
		t.Fatalf("installed provider = %q, want the root-owned binary %q", got, root)
	}
}

// An unknown home is the reason the root-owned tree is listed first, so the
// search must continue without a home instead of giving up.
func TestFreeBSDInstalledProviderFindsLibexecWithoutAHome(t *testing.T) {
	root := useWritableBSDProviderRoot(t)
	writeBSDProviderFile(t, root, 0o755)
	previous := bsdUserHomeDir
	bsdUserHomeDir = func() (string, error) { return "", errors.New("home unavailable") }
	t.Cleanup(func() { bsdUserHomeDir = previous })

	if got := installedBSDProvider(); got != root {
		t.Fatalf("installed provider = %q, want the root-owned binary %q", got, root)
	}
}
