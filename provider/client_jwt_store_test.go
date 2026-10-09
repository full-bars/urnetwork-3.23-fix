package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testClientId = "00000000-0000-0000-0000-000000000001"

func TestClientJWTStoreMissingFile(t *testing.T) {
	store := newClientJWTStore(filepath.Join(t.TempDir(), "does-not-exist.json"))

	_, ok := store.Get("proxy-1")
	if ok {
		t.Fatal("expected no entry from a missing store file")
	}
}

func TestClientJWTStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)

	_, ok := store.Get("proxy-1")
	if ok {
		t.Fatal("expected no entry from a corrupt store file")
	}
}

func TestClientJWTStorePutGetRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	entry := clientJWTEntry{
		ByClientJWT: createFakeJWTWithClaims(map[string]interface{}{
			"client_id": testClientId,
			"exp":       float64(time.Now().Unix() + 86400),
		}),
		ClientID: testClientId,
		MintedAt: time.Now(),
	}
	if err := store.Put("proxy-1", entry); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, ok := store.Get("proxy-1")
	if !ok {
		t.Fatal("expected entry after Put")
	}
	if got.ClientID != testClientId {
		t.Errorf("ClientID = %q, want %q", got.ClientID, testClientId)
	}

	// A fresh store instance reading the same file should see the same entry.
	reloaded := newClientJWTStore(path)
	got, ok = reloaded.Get("proxy-1")
	if !ok {
		t.Fatal("expected entry to survive a reload from disk")
	}
	if got.ClientID != testClientId {
		t.Errorf("reloaded ClientID = %q, want %q", got.ClientID, testClientId)
	}
}

func TestClientJWTStorePruneStaleEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Put("stale-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now().Add(-clientJWTStaleAfter - 24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("fresh-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	reloaded := newClientJWTStore(path)
	if _, ok := reloaded.Get("stale-proxy"); ok {
		t.Error("expected stale entry to be pruned on load")
	}
	if _, ok := reloaded.Get("fresh-proxy"); !ok {
		t.Error("expected fresh entry to survive pruning")
	}
}

func TestClientJWTStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Put("proxy-1", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("proxy-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := store.Get("proxy-1"); ok {
		t.Fatal("expected entry to be gone after Delete")
	}

	// The eviction must persist to disk, not just the in-memory map.
	reloaded := newClientJWTStore(path)
	if _, ok := reloaded.Get("proxy-1"); ok {
		t.Fatal("expected deletion to survive a reload from disk")
	}
}

func TestClientJWTStoreDeleteMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Delete("never-existed"); err != nil {
		t.Fatalf("Delete of a missing key should be a no-op, got: %v", err)
	}
}

func TestClientJWTStoreConcurrentPut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := filepath.Join("proxy", string(rune('a'+n%26)))
			_ = store.Put(key, clientJWTEntry{
				ByClientJWT: "irrelevant",
				ClientID:    testClientId,
				MintedAt:    time.Now(),
			})
		}(i)
	}
	wg.Wait()

	if _, ok := store.Get("proxy/a"); !ok {
		t.Error("expected at least one concurrent Put to have landed")
	}
}

func TestClientJWTStoreAnyNetworkID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("expected empty NetworkID from empty store, got %q", got)
	}

	_ = store.Put("p1", clientJWTEntry{
		ClientID: testClientId,
		MintedAt: time.Now(),
	})
	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("expected empty NetworkID when entries have no NetworkID, got %q", got)
	}

	_ = store.Put("p2", clientJWTEntry{
		ClientID:  testClientId,
		NetworkID: "net-test-123",
		MintedAt:  time.Now(),
	})
	if got := store.AnyNetworkID(); got != "net-test-123" {
		t.Fatalf("AnyNetworkID() = %q, want net-test-123", got)
	}

	// Conflicting network IDs in store must return empty (ambiguous)
	_ = store.Put("p3", clientJWTEntry{
		ClientID:  testClientId,
		NetworkID: "net-conflicting-456",
		MintedAt:  time.Now(),
	})
	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("AnyNetworkID() with conflicting networks = %q, want empty (ambiguous)", got)
	}
}

// Find is what lets an operator type a bare proxy address for a credentialed
// identity; its rules are exact key first, then a unique "address\x1f..." match,
// and never a guess between several.
func TestClientJWTStoreFind(t *testing.T) {
	store := newClientJWTStore(filepath.Join(t.TempDir(), ".client_jwts.json"))
	put := func(key string) {
		if err := store.Put(key, clientJWTEntry{ByClientJWT: "jwt-" + key}); err != nil {
			t.Fatal(err)
		}
	}
	put("10.0.0.1:1080\x1falice")
	put("10.0.0.2:1080\x1fbob")
	put("10.0.0.2:1080\x1fcarol")
	put("10.0.0.3:1080")
	put("10.0.0.3:1080\x1fdave")
	put("direct")

	if key, entry, cands := store.Find("direct"); key != "direct" || entry.ByClientJWT != "jwt-direct" || cands != nil {
		t.Fatalf("exact key: %q %+v %v", key, entry, cands)
	}
	if key, _, cands := store.Find("10.0.0.1:1080"); key != "10.0.0.1:1080\x1falice" || cands != nil {
		t.Fatalf("a unique credentialed match must resolve, got %q %v", key, cands)
	}
	if key, entry, cands := store.Find("10.0.0.2:1080"); key != "" || entry.ByClientJWT != "" || len(cands) != 2 {
		t.Fatalf("a shared address must return both candidates and no entry, got %q %+v %v", key, entry, cands)
	}
	if key, _, _ := store.Find("10.0.0.3:1080"); key != "10.0.0.3:1080" {
		t.Fatalf("an exact bare key must win over credentialed siblings, got %q", key)
	}
	if key, entry, cands := store.Find("10.0.0.9:1080"); key != "" || entry.ByClientJWT != "" || cands != nil {
		t.Fatalf("no match must be all zero, got %q %+v %v", key, entry, cands)
	}
	// a prefix of an address must not match another address
	if key, _, cands := store.Find("10.0.0.1:108"); key != "" || cands != nil {
		t.Fatalf("a partial address must not match, got %q %v", key, cands)
	}
}
