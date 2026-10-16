package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkClientJWTStorePuts measures one Put on a store holding a realistic
// fleet of entries (1000, rotating keys like 1000+ proxy identities minting
// and refreshing). The pre-write-behind store rewrote the whole file on every
// Put, which the canary alloc profile showed at about 11 percent of
// allocations; with the write-behind the Put is memory-only and the file is
// rewritten coalesced. Run the same benchmark before and after the change.
func BenchmarkClientJWTStorePuts(b *testing.B) {
	path := filepath.Join(b.TempDir(), "client_jwt_store.json")
	store := newClientJWTStore(path)

	const fleet = 1000
	ids := make([]string, fleet)
	for i := range ids {
		ids[i] = fmt.Sprintf("client-%04d", i)
	}
	for i, id := range ids {
		if err := store.Put(id, clientJWTEntry{
			ByClientJWT: fmt.Sprintf("jwt-%d", i),
			ClientID:    id,
			MintedAt:    time.Now(),
		}); err != nil {
			b.Fatalf("prefill put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ids[i%fleet]
		if err := store.Put(id, clientJWTEntry{
			ByClientJWT: "jwt-refresh",
			ClientID:    id,
			MintedAt:    time.Now(),
		}); err != nil {
			b.Fatalf("put: %v", err)
		}
	}
}
