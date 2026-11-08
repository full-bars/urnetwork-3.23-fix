//go:build linux

package main

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestConcurrentTrimAndReopenWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "log")
	f := openLogForTest(t, filepath.Join(t.TempDir(), "old"))
	var mu sync.Mutex
	var warned bool
	var when time.Time
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := trimRAMLogByRename(path, 10, 3)
			reportTrimFailure(&warned, &when, path, err)
		}()
		go func() {
			defer wg.Done()
			<-start
			c := checkLogPaths(path, &f, &mu, "", nil, nil, false, func(string) {})
			reportTrimFailure(&warned, &when, path, c.mainErr)
		}()
	}
	close(start)
	wg.Wait()
}

func TestTrimSwapRejectsStaleInterleaving(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f := openLogForTest(t, path)
	var mu sync.Mutex
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("trim"), 0644); err != nil {
		t.Fatal(err)
	}
	err := swapTrimmedLog(path, &f, &mu, func() (*os.File, error) {
		next, err := openSafeLog(path, 0644)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, path+".trimmed"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		changed, err := followLogPath(path, &f, &mu, 0644, reopenIfOrphaned)
		if err != nil || !changed {
			t.Fatalf("tick: %v %v", changed, err)
		}
		return next, nil
	})
	if err == nil {
		t.Fatal("stale trim descriptor installed")
	}
	if _, err := f.WriteString(" retained"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "new retained" {
		t.Fatalf("path %q %v", b, err)
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	drained := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := r.Read(buf)
		if err == nil {
			mu.Lock()
			_, err = f.Write(buf[:n])
			mu.Unlock()
		}
		drained <- err
	}()
	if _, err := w.Write([]byte("drained")); err != nil {
		t.Fatal(err)
	}
	err = <-drained
	close(release)
	<-done
	if err != nil {
		t.Fatal(err)
	}
}
