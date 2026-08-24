package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Coverage-gap tests (Sonnet round-3 review): writeProxyStateTo was 53%
// covered — only the happy path (MkdirAll ok, Marshal ok, CreateTemp ok,
// Write ok, Close ok, Rename ok) was exercised anywhere in the suite. These
// drive two of its real, reachable error returns black-box, without any
// production seam: a parent path that cannot become a directory, and a
// destination directory the process cannot write into.

// TestWriteProxyStateTo_MkdirAllError: when the parent directory of the
// target path cannot be created because a PATH COMPONENT is already a
// regular file (not a directory), os.MkdirAll fails with ENOTDIR and
// writeProxyStateTo must propagate that error rather than panicking or
// silently succeeding.
func TestWriteProxyStateTo_MkdirAllError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	// "blocker" is a FILE; asking to create a directory tree underneath it
	// must fail — MkdirAll cannot mkdir through a non-directory component.
	path := filepath.Join(blocker, "subdir", "proxy.state")

	err := writeProxyStateTo(path, &ProxyState{Proxies: map[string]ProxyEntry{}})
	if err == nil {
		t.Fatal("expected an error when the parent directory cannot be created (path component is a file)")
	}
	// The bogus path must not have somehow been written.
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("proxy.state must not exist after a failed write")
	}
}

// TestWriteProxyStateTo_CreateTempError: when the destination directory
// exists but the process has no write permission on it, os.CreateTemp
// (which creates a NEW file inside that directory) fails, and
// writeProxyStateTo must return that error instead of the MkdirAll no-op
// success masking it.
func TestWriteProxyStateTo_CreateTempError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission bits")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "state_dir")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0500); err != nil { // r-x: readable/listable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0700) }) // let t.TempDir() clean up afterward

	path := filepath.Join(sub, "proxy.state")
	err := writeProxyStateTo(path, &ProxyState{Proxies: map[string]ProxyEntry{}})
	if err == nil {
		t.Fatal("expected CreateTemp to fail in a directory without write permission")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("proxy.state must not exist after a failed write")
	}
}

// TestWriteProxyStateTo_HappyPathStillWorks is a sanity companion: the two
// error-path tests above must not have broken (or coincidentally relied on
// breaking) the ordinary success case, which round-trips through
// readProxyStateFrom.
func TestWriteProxyStateTo_HappyPathStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "proxy.state")
	want := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {ID: 1, Health: "up"},
	}}
	if err := writeProxyStateTo(path, want); err != nil {
		t.Fatalf("unexpected error on the happy path: %v", err)
	}
	got, err := readProxyStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxies["1.2.3.4:1080"].ID != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
