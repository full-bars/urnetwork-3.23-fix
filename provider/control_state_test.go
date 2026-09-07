package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestControlState_SetGetClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	if _, found := s.get("node_name"); found {
		t.Fatalf("expected not found before any set")
	}

	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("got (%q, %v), want (%q, true)", v, found, "nyc-1")
	}

	if err := s.clear("node_name"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, found := s.get("node_name"); found {
		t.Fatalf("expected not found after clear")
	}
}

func TestControlState_UnknownKeyRejected(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	if err := s.set("not-a-real-key", "value"); err == nil {
		t.Fatalf("expected error for unknown key on set")
	}
	if err := s.clear("not-a-real-key"); err == nil {
		t.Fatalf("expected error for unknown key on clear")
	}
}

func TestControlState_PersistAndLoad_Roundtrip(t *testing.T) {
	home := withTempHome(t)
	s := newControlState()

	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.set("fast_auth", "on"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	loaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	if v, found := loaded.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v)", v, found)
	}
	if v, found := loaded.get("fast_auth"); !found || v != "on" {
		t.Fatalf("fast_auth: got (%q, %v)", v, found)
	}

	path := filepath.Join(home, ".urnetwork", "provider_state.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat provider_state.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("provider_state.json perms = %o, want 0600", perm)
	}
}

func TestLoadControlState_MissingFileIsEmptyNotError(t *testing.T) {
	withTempHome(t)

	s, err := loadControlState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, found := s.get("node_name"); found {
		t.Fatalf("expected empty state for a fresh box with no provider_state.json")
	}
}

// TestLoadControlState_UnknownKeyIsDroppedNotFatal covers a downgrade: a
// newer provider binary wrote a key this one doesn't recognize. Startup
// must not fail over it — the provider is the only writer, so this means
// "rolled back to an older binary," not "the file is corrupt."
func TestLoadControlState_UnknownKeyIsDroppedNotFatal(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"node_name":"nyc-1","some_future_key":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "provider_state.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := loadControlState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v)", v, found)
	}
}

// TestControlState_ConcurrentSetClear exercises the mutex under -race: many
// goroutines setting/clearing different keys at once must never corrupt the
// map or race on it — there's no read-modify-write hazard here (unlike the
// file-based design this replaces) because every method holds the lock for
// its entire body, but the persisted file's snapshot must still always be
// self-consistent.
func TestControlState_ConcurrentSetClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	keys := []string{
		"node_name", "report_url", "report_interval", "fast_auth",
		"proxy_self_heal", "proxy_url_max", "proxy_url_refresh",
		"proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval",
	}

	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.set(key, "v")
				s.get(key)
				s.persist()
			}
			s.clear(key)
		}(k)
	}
	wg.Wait()

	for _, k := range keys {
		if _, found := s.get(k); found {
			t.Errorf("key %s: expected cleared, still found", k)
		}
	}
}

// TestControlState_ConcurrentSetAndPersist_DiskMatchesMemory guards against
// the race setAndPersist/clearAndPersist were introduced to close: when
// set() and persist() were two separately-locked calls (as
// control_socket.go's handlers used to do it), a concurrent persist() for a
// different key could write this key's not-yet-confirmed value to disk
// before this key's own persist() call decided whether to roll it back —
// leaving disk holding a value memory no longer agreed with. Because
// setAndPersist now holds the lock across mutate-and-write, every concurrent
// writer's disk snapshot is a consistent superset of the memory state at the
// instant it ran, so after all goroutines finish, disk must exactly match
// the final in-memory state.
func TestControlState_ConcurrentSetAndPersist_DiskMatchesMemory(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	keys := []string{
		"node_name", "report_url", "report_interval", "fast_auth",
		"proxy_self_heal", "proxy_url_max", "proxy_url_refresh",
		"proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval",
	}

	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		go func(key, val string) {
			defer wg.Done()
			if err := s.setAndPersist(key, val); err != nil {
				t.Errorf("setAndPersist(%q): %v", key, err)
			}
		}(k, fmt.Sprintf("v%d", i))
	}
	wg.Wait()

	loaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	for i, k := range keys {
		want := fmt.Sprintf("v%d", i)
		memVal, memFound := s.get(k)
		diskVal, diskFound := loaded.get(k)
		if !memFound || memVal != want {
			t.Errorf("in-memory %q = (%q, %v), want (%q, true)", k, memVal, memFound, want)
		}
		if !diskFound || diskVal != want {
			t.Errorf("on-disk %q = (%q, %v), want (%q, true) — disk/memory disagreement", k, diskVal, diskFound, want)
		}
	}
}
