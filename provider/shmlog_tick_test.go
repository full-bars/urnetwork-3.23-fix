//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLogPathTick(t *testing.T) {
	dir := t.TempDir()
	mainPath, impPath := filepath.Join(dir, "main"), filepath.Join(dir, "important")
	open := func(path string) *os.File {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	main, imp := open(mainPath), open(impPath)
	defer func() { main.Close(); imp.Close() }()
	var mainMu, impMu sync.Mutex
	var notices []string
	report := func(path string) {
		if !mainMu.TryLock() {
			t.Fatal("main lock held during report")
		}
		mainMu.Unlock()
		if !impMu.TryLock() {
			t.Fatal("important lock held during report")
		}
		impMu.Unlock()
		notices = append(notices, path)
	}
	tick := func(path string, enabled bool) logPathChanges {
		return checkLogPaths(mainPath, &main, &mainMu, path, &imp, &impMu, enabled, report)
	}
	original := main
	if c := tick(impPath, true); c != (logPathChanges{}) || main != original || len(notices) != 0 {
		t.Fatalf("unchanged tick: %+v %v", c, notices)
	}
	if _, err := main.WriteString(strings.Repeat("line of log\n", 20)); err != nil {
		t.Fatal(err)
	}
	if ok, err := trimRAMLogByRename(mainPath, 64, 3); err != nil || !ok {
		t.Fatalf("trim: %v %v", ok, err)
	}
	if c := tick(impPath, true); !c.main || c.important || c.mainErr != nil {
		t.Fatalf("trim tick: %+v", c)
	}
	if _, err := main.WriteString("after trim\n"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(mainPath)
	if err != nil || !strings.Contains(string(b), "after trim") {
		t.Fatalf("write after trim: %q %v", b, err)
	}
	if err := os.Remove(mainPath); err != nil {
		t.Fatal(err)
	}
	if c := tick(impPath, true); !c.main || c.mainErr != nil {
		t.Fatalf("removed tick: %+v", c)
	}
	if _, err := main.WriteString("recreated\n"); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(mainPath)
	if err != nil || string(b) != "recreated\n" {
		t.Fatalf("recreated: %q %v", b, err)
	}
	if err := os.Remove(impPath); err != nil {
		t.Fatal(err)
	}
	oldImp := imp
	if c := tick(shmImportantLogPath, true); c.important || imp != oldImp {
		t.Fatalf("fallback should not reopen: %+v", c)
	}
	if c := tick(impPath, false); c.important || imp != oldImp {
		t.Fatalf("disabled should not reopen: %+v", c)
	}
	if c := tick(impPath, true); !c.important || c.importantErr != nil {
		t.Fatalf("important tick: %+v", c)
	}
	if _, err := imp.WriteString("important\n"); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(impPath)
	if err != nil || string(b) != "important\n" {
		t.Fatalf("important write: %q %v", b, err)
	}
	if len(notices) != 3 || notices[0] != mainPath || notices[1] != mainPath || notices[2] != impPath {
		t.Fatalf("notices: %v", notices)
	}
}
