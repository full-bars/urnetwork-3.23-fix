//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/docopt/docopt-go"
)

type chownCall struct {
	path     string
	uid, gid int
}

func stubChown(t *testing.T, fn func(string, int, int) error) *[]chownCall {
	t.Helper()
	var calls []chownCall
	prev := chownFile
	chownFile = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return fn(path, uid, gid)
	}
	t.Cleanup(func() { chownFile = prev })
	return &calls
}

func writeOwnerSource(t *testing.T) string {
	t.Helper()
	withTempHome(t)
	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(sourcePath, []byte("1.1.1.1:1080:alice:old\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	return sourcePath
}

// A rotation hands the replacement to the source's owner, before the rename.
// Without it a root-run proxy add leaves the service unable to read its file.
func TestProxyAddFileBackedKeepsSourceOwner(t *testing.T) {
	sourcePath := writeOwnerSource(t)
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	calls := stubChown(t, func(string, int, int) error { return nil })

	proxyAdd(docopt.Opts{"--file": sourcePath, "<key_address>": []string{"1.1.1.1:1080:alice:new"}})

	if len(*calls) != 1 {
		t.Fatalf("chown calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.uid != int(st.Uid) || c.gid != int(st.Gid) {
		t.Fatalf("chown to %d:%d, want source owner %d:%d", c.uid, c.gid, st.Uid, st.Gid)
	}
	if filepath.Dir(c.path) != filepath.Dir(sourcePath) || c.path == sourcePath {
		t.Fatalf("chown applied to %s, want replacement beside source", c.path)
	}
	b, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "1.1.1.1:1080:alice:new") {
		t.Fatalf("new proxy was not written:\n%s", b)
	}
	infoAfter, _ := os.Stat(sourcePath)
	if infoAfter.Mode().Perm() != 0640 {
		t.Fatalf("source perm = %04o, want 0640 preserved", infoAfter.Mode().Perm())
	}
}

// When the owner cannot be kept the source is left as it was and no temp file remains.
func TestProxyAddFileBackedFailsWhenOwnerCannotBeKept(t *testing.T) {
	sourcePath := writeOwnerSource(t)
	stubChown(t, func(string, int, int) error { return errors.New("operation not permitted") })

	proxyAdd(docopt.Opts{"--file": sourcePath, "<key_address>": []string{"1.1.1.1:1080:alice:new"}})

	b, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "1.1.1.1:1080:alice:old\n" {
		t.Fatalf("source changed despite chown failure:\n%s", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(sourcePath))
	for _, e := range entries {
		if e.Name() != "proxies.txt" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}
