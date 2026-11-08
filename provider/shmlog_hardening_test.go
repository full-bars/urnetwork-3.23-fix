//go:build linux

package main

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSafeReopenRejectsHostilePath(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "mode"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			old := openLogForTest(t, path)
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(path, nil, 0666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			changes := checkLogPaths(path, &old, &mu, "", nil, nil, false, func(string) { t.Error("reported reopen") })
			if changes.mainErr == nil || changes.main {
				t.Fatalf("expected refused path: %+v", changes)
			}
			if _, err := old.WriteString("still works"); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path + ".old")
			if err != nil || string(b) != "still works" {
				t.Fatalf("old file %q %v", b, err)
			}
		})
	}
}

func TestSlowReopenDoesNotBlockWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f := openLogForTest(t, path)
	var mu sync.Mutex
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		followLogPath(path, &f, &mu, 0644, func(string, *os.File, os.FileMode) (*os.File, bool, error) {
			close(started)
			<-release
			return nil, false, nil
		})
	}()
	<-started
	mu.Lock()
	_, err := f.WriteString("drained")
	mu.Unlock()
	close(release)
	<-done
	if err != nil {
		t.Fatal(err)
	}
}
