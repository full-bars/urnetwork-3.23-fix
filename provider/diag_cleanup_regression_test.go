package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagRetentionIncludesNewBundle(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	root := diagRoot(env)
	for _, name := range []string{"20261101T000000Z", "20261101T010000Z", "20261101T020000Z", "20261101T030000Z", "20261101T040000Z"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := len(listDiagBundles(root)); got != env.keep {
		t.Fatalf("got %d bundles, want %d", got, env.keep)
	}
}

func TestDiagOrphanCleanupPreservesUnrelatedPartial(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	root := diagRoot(env)
	for _, name := range []string{"20261102T000000Z.partial", "operator-notes.partial"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	removeDiagOrphans(root)
	if _, err := os.Stat(filepath.Join(root, "operator-notes.partial")); err != nil {
		t.Fatalf("unrelated data removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "20261102T000000Z.partial")); !os.IsNotExist(err) {
		t.Fatalf("orphan survived: %v", err)
	}
}

func TestDiagChangedStaleLockRemainsBusy(t *testing.T) {
	env, clock := newDiagTestEnv(t)
	root := diagRoot(env)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, diagLock)
	stale := "pid=100 started=" + clock.t.Add(-time.Hour).Format(time.RFC3339) + "\n"
	fresh := "pid=200 started=" + clock.t.Format(time.RFC3339) + "\n"
	if err := os.WriteFile(path, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	env.now = func() time.Time {
		calls++
		if calls == 2 {
			if err := os.WriteFile(path, []byte(fresh), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return clock.t
	}
	release, _, err := acquireDiagLock(root, env)
	if release != nil {
		defer release()
	}
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("changed lock should be busy: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != fresh {
		t.Fatalf("replacement lock changed: %q %v", got, err)
	}
}
