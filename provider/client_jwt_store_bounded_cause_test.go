package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests that each of the 5 cause values (initial, added, refreshed, expired, removed)
// is produced by a real code path, that replaced count is reported accurately,
// and that the log line format matches the specification.
func TestClientJWTStoreSaveCauseLogging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	baseTime := time.Now()
	clock := baseTime
	store.clock = func() time.Time {
		return clock
	}

	validJWT1 := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(baseTime.Add(24 * time.Hour).Unix()),
	})
	validJWT2 := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(baseTime.Add(48 * time.Hour).Unix()),
	})
	expiredJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(baseTime.Add(-2 * time.Hour).Unix()),
	})
	renewedJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(baseTime.Add(72 * time.Hour).Unix()),
	})

	// 1. Initial: first write when store does not exist on disk yet.
	out := captureTlog(t, func() {
		err := store.Put("proxy-1", clientJWTEntry{
			ByClientJWT: validJWT1,
			ClientID:    testClientId,
			NetworkID:   "net-test",
			MintedAt:    clock,
		})
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 1 entries cause=initial replaced=0 duration=0ms") {
		t.Fatalf("expected initial save cause, got:\n%s", out)
	}

	// 2. Added: store exists, adding a new identity key.
	out = captureTlog(t, func() {
		err := store.Put("proxy-2", clientJWTEntry{
			ByClientJWT: validJWT1,
			ClientID:    testClientId,
			NetworkID:   "net-test",
			MintedAt:    clock,
		})
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 2 entries cause=added replaced=0 duration=0ms") {
		t.Fatalf("expected added save cause, got:\n%s", out)
	}

	// 3. Refreshed: updating an existing identity whose current token is not expired.
	out = captureTlog(t, func() {
		err := store.Put("proxy-1", clientJWTEntry{
			ByClientJWT: validJWT2,
			ClientID:    testClientId,
			NetworkID:   "net-test",
			MintedAt:    clock,
		})
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 2 entries cause=refreshed replaced=1 duration=0ms") {
		t.Fatalf("expected refreshed save cause, got:\n%s", out)
	}

	// Add an expired entry so we can test replacing an expired token.
	out = captureTlog(t, func() {
		err := store.Put("proxy-expired", clientJWTEntry{
			ByClientJWT: expiredJWT,
			ClientID:    testClientId,
			NetworkID:   "net-test",
			MintedAt:    clock.Add(-3 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 3 entries cause=added replaced=0 duration=0ms") {
		t.Fatalf("expected added save cause for proxy-expired, got:\n%s", out)
	}

	// 4. Expired: updating an existing identity whose current token in baseline is expired.
	out = captureTlog(t, func() {
		err := store.Put("proxy-expired", clientJWTEntry{
			ByClientJWT: renewedJWT,
			ClientID:    testClientId,
			NetworkID:   "net-test",
			MintedAt:    clock,
		})
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 3 entries cause=expired replaced=1 duration=0ms") {
		t.Fatalf("expected expired save cause, got:\n%s", out)
	}

	// 5. Removed: deleting an existing identity key.
	out = captureTlog(t, func() {
		err := store.Delete("proxy-2")
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: save 2 entries cause=removed replaced=0 duration=0ms") {
		t.Fatalf("expected removed save cause, got:\n%s", out)
	}
}

// Tests that elapsed duration in duration=<ms> is properly measured and formatted.
func TestClientJWTStoreSaveDurationFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	baseTime := time.Now()
	callCount := 0
	store.clock = func() time.Time {
		callCount++
		// First call is start time in flushBatchLocked; second call is when measuring dur.
		if callCount == 1 {
			return baseTime
		}
		return baseTime.Add(42 * time.Millisecond)
	}

	out := captureTlog(t, func() {
		err := store.Put("proxy-1", clientJWTEntry{
			ByClientJWT: "token-1",
			ClientID:    testClientId,
			MintedAt:    baseTime,
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, "duration=42ms") {
		t.Fatalf("expected duration=42ms, got:\n%s", out)
	}
}
