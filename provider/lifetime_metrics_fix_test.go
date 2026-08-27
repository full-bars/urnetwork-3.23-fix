package main

import (
	"testing"
)

// TestU64AtCreateIfAbsent guards the lifetime-collector idle-branch fix: the
// first tick's re-anchor loop runs over a populated proxy set (bw) while the
// the prev-less baseline maps are still empty, so it MUST create the slot via
// u64At — a bare map index dereference would be a nil *uint64 panic that
// crashes the provider process (Sonnet CRITICAL, 2026-08-27). This exercises
// exactly that mechanism: u64At on an empty map returns an assignable slot the
// assignment can write without panicking.
func TestU64AtCreateIfAbsent(t *testing.T) {
	prev := map[string]*uint64{}
	slot := u64At(prev, "proxy-a")
	if slot == nil {
		t.Fatal("u64At must return a usable pointer for a missing key")
	}
	*slot = 12345 // the same assignment the idle branch does
	got := prev["proxy-a"]
	if got == nil {
		t.Fatal("u64At must register the key in the map")
	}
	if *got != 12345 {
		t.Fatalf("slot value after write = %d, want 12345", *got)
	}
	// A second call must reuse the same slot (no re-creation).
	if sl2 := u64At(prev, "proxy-a"); sl2 != slot {
		t.Fatal("u64At on an existing key must return the same slot")
	}
}
