package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Helpers for the write-behind tests: a shortened flush cadence (restored at
// test end) and a bounded barrier on the flush observer.

func setFlushCadence(store *clientJWTStore, interval time.Duration, threshold int) {
	store.flushInterval = interval
	store.flushThreshold = threshold
}

func newTestEntry(id string) clientJWTEntry {
	return clientJWTEntry{
		ByClientJWT: "fake",
		ClientID:    id,
		MintedAt:    time.Now(),
	}
}

func awaitFlush(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no flush observed", what)
	}
}

func reloadStore(t *testing.T, path string) map[string]clientJWTEntry {
	t.Helper()
	store := newClientJWTStore(path)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loadLocked()
	out := map[string]clientJWTEntry{}
	for k, v := range store.entries {
		out[k] = v
	}
	return out
}

// Many Puts between flushes cost ONE rewrite of the file: the write-count
// assertion that kills the O(N squared) startup regression.
func TestJWTWriteBehindOneWriteForManyPuts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Hour, 1<<30)
	t.Cleanup(store.Close)

	for i := 0; i < 5; i++ {
		if err := store.Put("proxy-"+string(rune('a'+i)), newTestEntry(testClientId)); err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	store.mu.Lock()
	flushes := store.flushes
	store.mu.Unlock()
	if flushes != 1 {
		t.Fatalf("5 Puts must cost one flush, got %d", flushes)
	}
	if got := reloadStore(t, path); len(got) != 5 {
		t.Fatalf("reloaded %d entries, want 5", len(got))
	}
}

// The tick lands pending Puts without any explicit call.
func TestJWTWriteBehindTickFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, 2*time.Millisecond, 1<<30)
	t.Cleanup(store.Close)
	observer := make(chan struct{}, 16)
	store.flushObserverForTest = observer

	for i := 0; i < 3; i++ {
		if err := store.Put("proxy-"+string(rune('a'+i)), newTestEntry(testClientId)); err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
	}
	awaitFlush(t, observer, "tick")
	if got := reloadStore(t, path); len(got) != 3 {
		t.Fatalf("reloaded %d entries, want 3", len(got))
	}
}

// The crash window: a Put after the last flush is not on disk until the next
// flush. The test asserts the window, not staleness beyond it.
func TestJWTWriteBehindCrashWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Hour, 1<<30)
	t.Cleanup(store.Close)

	if err := store.Put("flushed", newTestEntry(testClientId)); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if err := store.Put("pending", newTestEntry(testClientId)); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got := reloadStore(t, path)
	if _, ok := got["flushed"]; !ok {
		t.Fatal("the flushed entry must be on disk")
	}
	if _, ok := got["pending"]; ok {
		t.Fatal("a Put after the last flush must not be on disk yet")
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}
	if _, ok := reloadStore(t, path)["pending"]; !ok {
		t.Fatal("the pending entry must be on disk after the next flush")
	}
}

// The dirty threshold kicks a flush without waiting for the tick.
func TestJWTWriteBehindThresholdKicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Hour, 4)
	t.Cleanup(store.Close)
	observer := make(chan struct{}, 16)
	store.flushObserverForTest = observer

	for i := 0; i < 4; i++ {
		if err := store.Put("proxy-"+string(rune('a'+i)), newTestEntry(testClientId)); err != nil {
			t.Fatalf("Put %d failed: %v", i, err)
		}
	}
	awaitFlush(t, observer, "threshold")
	if got := reloadStore(t, path); len(got) != 4 {
		t.Fatalf("reloaded %d entries, want 4", len(got))
	}
}

// Close flushes and then refuses further Puts.
func TestJWTWriteBehindCloseFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Hour, 1<<30)

	if err := store.Put("last", newTestEntry(testClientId)); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	store.Close()
	if _, ok := reloadStore(t, path)["last"]; !ok {
		t.Fatal("Close must flush the pending entry")
	}
	if err := store.Put("after-close", newTestEntry(testClientId)); err == nil {
		t.Fatal("a Put after Close must fail")
	}
	store.Close() // idempotent
}

// A Delete of a still-pending Put must not resurrect the key on the flush.
func TestJWTWriteBehindDeleteCannotResurrect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Hour, 1<<30)
	t.Cleanup(store.Close)

	if err := store.Put("doomed", newTestEntry(testClientId)); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := store.Delete("doomed"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if _, ok := reloadStore(t, path)["doomed"]; ok {
		t.Fatal("Delete must win over the pending Put on the same flush")
	}
	store.mu.Lock()
	flushes := store.flushes
	store.mu.Unlock()
	if flushes != 1 {
		t.Fatalf("a Put and a Delete must cost one flush, got %d", flushes)
	}
}

// Concurrent Puts and flushes stay consistent under the race detector.
func TestJWTWriteBehindConcurrentPutsAndFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)
	setFlushCadence(store, time.Millisecond, 8)
	t.Cleanup(store.Close)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				key := "proxy-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10)) + string(rune('0'+i/10))
				_ = store.Put(key, newTestEntry(testClientId))
				if i%7 == 0 {
					_ = store.Flush()
				}
			}
		}(w)
	}
	wg.Wait()
	if err := store.Flush(); err != nil {
		t.Fatalf("final Flush failed: %v", err)
	}
	if got := reloadStore(t, path); len(got) != 100 {
		t.Fatalf("reloaded %d entries, want 100", len(got))
	}
}
