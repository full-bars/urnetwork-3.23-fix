//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func mkInstall(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, n := range []string{"urnet-tools", "urnetwork"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestLinkToolsIntoDirCreatesAndIsIdempotent(t *testing.T) {
	src, dir := mkInstall(t), filepath.Join(t.TempDir(), "bin")
	changed, err := linkToolsIntoDir(dir, src)
	if err != nil || len(changed) != 3 {
		t.Fatalf("first run: changed=%v err=%v", changed, err)
	}
	// urtop is not a binary of its own: it is a name urnet-tools answers to, so
	// it points at the urnet-tools binary.
	for link, target := range map[string]string{"urnet-tools": "urnet-tools", "urnetwork": "urnetwork", "urtop": "urnet-tools"} {
		if got, _ := os.Readlink(filepath.Join(dir, link)); got != filepath.Join(src, target) {
			t.Fatalf("%s -> %q, want %q", link, got, filepath.Join(src, target))
		}
	}
	if changed, err := linkToolsIntoDir(dir, src); err != nil || len(changed) != 0 {
		t.Fatalf("second run must be a quiet no-op: changed=%v err=%v", changed, err)
	}
}

func TestLinkToolsIntoDirNeverClobbersAndRepointsStale(t *testing.T) {
	src, dir := mkInstall(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "urnet-tools"), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/urnetwork", filepath.Join(dir, "urnetwork")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	changed, err := linkToolsIntoDir(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "urnet-tools")); string(b) != "mine" {
		t.Fatalf("a real file was overwritten: %q", b)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "urnetwork")); got != filepath.Join(src, "urnetwork") {
		t.Fatalf("stale link not repointed: %q", got)
	}
	if len(changed) != 2 || changed[0] != "urnetwork" || changed[1] != "urtop" {
		t.Fatalf("changed = %v, want urnetwork (repointed) and urtop (new); urnet-tools is not ours", changed)
	}
}

// An install that predates urtop has the two old links and no third. Running
// the link step again (which `urnet-tools update` does) adds only urtop, and a
// stale or foreign urtop is treated like any other link.
func TestLinkToolsIntoDirAddsUrtopToAnOlderInstall(t *testing.T) {
	src, dir := mkInstall(t), t.TempDir()
	for _, n := range []string{"urnet-tools", "urnetwork"} {
		if err := os.Symlink(filepath.Join(src, n), filepath.Join(dir, n)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
	}
	changed, err := linkToolsIntoDir(dir, src)
	if err != nil || len(changed) != 1 || changed[0] != "urtop" {
		t.Fatalf("an older install should gain only urtop: changed=%v err=%v", changed, err)
	}

	// A urtop left pointing at an old install path is repointed at urnet-tools.
	if err := os.Remove(filepath.Join(dir, "urtop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/urnet-tools", filepath.Join(dir, "urtop")); err != nil {
		t.Fatal(err)
	}
	if changed, err := linkToolsIntoDir(dir, src); err != nil || len(changed) != 1 || changed[0] != "urtop" {
		t.Fatalf("stale urtop should be repointed: changed=%v err=%v", changed, err)
	}

	// Someone else's real urtop is never overwritten.
	os.Remove(filepath.Join(dir, "urtop"))
	if err := os.WriteFile(filepath.Join(dir, "urtop"), []byte("theirs"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkToolsIntoDir(dir, src)
	if b, _ := os.ReadFile(filepath.Join(dir, "urtop")); string(b) != "theirs" {
		t.Fatalf("a real urtop file was overwritten: %q", b)
	}
}

func TestLinkToolsIntoDirSkipsTheInstallDirItself(t *testing.T) {
	src := mkInstall(t)
	if changed, err := linkToolsIntoDir(src, src); err != nil || len(changed) != 0 {
		t.Fatalf("linking a dir into itself must do nothing: %v %v", changed, err)
	}
}

// Under `go test` the executable is not called urnet-tools, so the self-heal
// must not touch the developer's real ~/.local/bin or /usr/local/bin.
func TestEnsureToolOnPathIgnoresNonToolBinaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ensureToolOnPath()
	if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
		t.Fatal("ensureToolOnPath created files for a non-urnet-tools executable")
	}
}
