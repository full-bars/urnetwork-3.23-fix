//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidsCgroupPath(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"0::/user.slice/session.scope", "/user.slice/session.scope"},
		{"4:cpu,pids:/test", "/test"},
		{"5:cpu:/wrong", ""},
		{"bad", ""},
	} {
		if got := pidsCgroupPath(c.line); got != c.want {
			t.Errorf("%q: %q", c.line, got)
		}
	}
}

func TestPidsMaxWithinInheritsTightestLimit(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "parent", "child")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "parent", "pids.max"), []byte("100\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"max", "200", "50"} {
		if err := os.WriteFile(filepath.Join(child, "pids.max"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		want := uint64(100)
		if value == "50" {
			want = 50
		}
		if got := pidsMaxWithin(root, "/parent/child"); got != want {
			t.Fatalf("child %s: got %d, want %d", value, got, want)
		}
	}
}
