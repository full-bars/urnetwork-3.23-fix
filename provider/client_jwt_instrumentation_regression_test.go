package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJWTStoreFailedRenameNotCompleted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	s := newClientJWTStore(p)
	out := captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err == nil {
			t.Fatal("expected failure")
		}
	})
	if s.attempts != 1 || s.flushes != 0 || strings.Contains(out, "[jwt-store] write") || strings.Contains(out, "client_jwt_store: save") {
		t.Fatalf("failed rename counted as completed: flushes=%d log=%s", s.flushes, out)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".client_jwts-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp file leaked: %v", matches)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	out = captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "attempts=2 completed=1") {
		t.Fatalf("wrong retry counters: %s", out)
	}
}

func TestJWTStorePrunedDiskEntriesStayPruned(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store")
	now := time.Now()
	fresh := clientJWTEntry{MintedAt: now}
	stale := clientJWTEntry{MintedAt: now.Add(-clientJWTStaleAfter - time.Hour)}
	b, _ := json.MarshalIndent(map[string]clientJWTEntry{"fresh": fresh, "stale": stale}, "", "  ")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	s := newClientJWTStore(p)
	if err := s.Put("new", fresh); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]clientJWTEntry
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["stale"]; ok {
		t.Fatal("stale disk entry resurrected")
	}
	if !got["new"].MintedAt.Equal(fresh.MintedAt) {
		t.Fatal("mint time changed")
	}
}

func TestJWTStoreBoundedActualChangeCause(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store")
	s := newClientJWTStore(p)
	e := clientJWTEntry{MintedAt: time.Now()}
	if err := s.Put("old", e); err != nil {
		t.Fatal(err)
	}
	out := captureTlog(t, func() {
		if err := s.flushBatchLocked(map[string]clientJWTEntry{"new": e}, []string{"old"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "cause=added_removed") {
		t.Fatalf("missing bounded actual-change cause: %s", out)
	}
}

func TestJWTStoreRepeatedWritesAndSkipsAreLogged(t *testing.T) {
	s := newClientJWTStore(filepath.Join(t.TempDir(), "store"))
	e := clientJWTEntry{MintedAt: time.Now()}
	if err := s.Put("x", e); err != nil {
		t.Fatal(err)
	}
	out := captureTlog(t, func() {
		for i := 0; i < 3; i++ {
			if err := s.Put("x", e); err != nil {
				t.Fatal(err)
			}
		}
	})
	if n := strings.Count(out, "skip write"); n != 3 {
		t.Fatalf("got %d skip logs, want 3: %s", n, out)
	}
	out = captureTlog(t, func() {
		for i := 0; i < 3; i++ {
			e.ByClientJWT += "x"
			if err := s.Put("x", e); err != nil {
				t.Fatal(err)
			}
		}
	})
	if n := strings.Count(out, "client_jwt_store: save"); n != 3 {
		t.Fatalf("got %d save logs, want 3: %s", n, out)
	}
}

func TestJWTStoreSaveHasCompletionCountersAndDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := newClientJWTStore(path)
	calls := 0
	s.clock = func() time.Time {
		calls++
		if calls == 2 {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("completion clock before rename: %v", err)
			}
		}
		return time.Unix(100, int64(calls)*int64(time.Millisecond))
	}
	out := captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "duration=1ms") || !strings.Contains(out, "attempts=1 completed=1") {
		t.Fatalf("missing completion instrumentation: %s", out)
	}
}
