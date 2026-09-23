package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func authSettings(address, user, password string) *connect.ProxySettings {
	return &connect.ProxySettings{
		Network: "tcp",
		Address: address,
		Auth:    &proxy.Auth{User: user, Password: password},
	}
}

func noAuthSettings(address string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: address}
}

func TestProxyKeyDisplay_NeverShowsRawSeparatorOrPassword(t *testing.T) {
	s := authSettings("dc.decodo.com:10058", "sppmr4vcnj", "super-secret-password")
	got := proxyKeyDisplay(s.Key())
	if strings.Contains(got, "\x1f") {
		t.Errorf("proxyKeyDisplay leaked the raw \\x1f separator: %q", got)
	}
	if strings.Contains(got, "super-secret-password") {
		t.Errorf("proxyKeyDisplay leaked the password: %q", got)
	}
	if !strings.Contains(got, "dc.decodo.com:10058") {
		t.Errorf("proxyKeyDisplay dropped the address: %q", got)
	}
}

func TestProxyKeyDisplay_NoAuthIsJustTheAddress(t *testing.T) {
	s := noAuthSettings("1.2.3.4:1080")
	if got := proxyKeyDisplay(s.Key()); got != "1.2.3.4:1080" {
		t.Errorf("proxyKeyDisplay(no-auth) = %q, want bare address", got)
	}
}

// TestAdoptLegacyProxyState_SingleIdentityCarriesOver is the ordinary case:
// one address, one account, that account was already tracked under the
// bare-address key. Adoption must move ID and history to the new key
// without losing or fabricating anything.
func TestAdoptLegacyProxyState_SingleIdentityCarriesOver(t *testing.T) {
	const addr = "1.2.3.4:1080"
	legacy := ProxyEntry{ID: 42, Health: "up", AuthFailures: 3}
	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: legacy}}

	desired := []*connect.ProxySettings{authSettings(addr, "alice", "pw1")}
	adopted, split := adoptLegacyProxyState(state, desired)

	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0", adopted, split)
	}
	if _, stillLegacy := state.Proxies[addr]; stillLegacy {
		t.Errorf("legacy key %q still present after adoption", addr)
	}
	newKey := desired[0].Key()
	got, ok := state.Proxies[newKey]
	if !ok {
		t.Fatalf("new key %q missing after adoption", newKey)
	}
	if got.ID != 42 || got.Health != "up" || got.AuthFailures != 3 {
		t.Errorf("adopted entry lost data: %+v, want ID=42 Health=up AuthFailures=3", got)
	}
}

// TestAdoptLegacyProxyState_NoAuthIsANoOp covers the load-bearing migration
// lever: an unauthenticated proxy's Key() equals its Address, so there is
// nothing to move — the legacy entry is already correctly keyed.
func TestAdoptLegacyProxyState_NoAuthIsANoOp(t *testing.T) {
	const addr = "1.2.3.4:1080"
	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: {ID: 7}}}

	adopted, split := adoptLegacyProxyState(state, []*connect.ProxySettings{noAuthSettings(addr)})

	if adopted != 0 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 0,0 for a no-auth proxy", adopted, split)
	}
	if got := state.Proxies[addr]; got.ID != 7 {
		t.Errorf("no-auth entry was disturbed: %+v", got)
	}
}

// TestAdoptLegacyProxyState_SplitPicksDeterministicWinner is the actual
// production bug: two Decodo accounts sharing one gateway address. The
// legacy entry (ID/history) must go to exactly one identity, chosen the
// same way every time regardless of map/slice iteration order, and the
// other identity must NOT inherit any of it (that would silently give a
// brand-new account fabricated history).
func TestAdoptLegacyProxyState_SplitPicksDeterministicWinner(t *testing.T) {
	const addr = "dc.decodo.com:10058"
	legacy := ProxyEntry{ID: 99, Health: "up", AuthFailures: 12}

	userA := authSettings(addr, "sppmr4vcnj", "pw1")
	userB := authSettings(addr, "user-sppmr4vcnj-country-us-city-metro", "pw2")
	wantWinner := userA.Key() // "dc.decodo.com:10058\x1fsppmr4vcnj" sorts before the "user-..." key

	for _, order := range [][]*connect.ProxySettings{{userA, userB}, {userB, userA}} {
		state := &ProxyState{Proxies: map[string]ProxyEntry{addr: legacy}}
		adopted, split := adoptLegacyProxyState(state, order)

		if adopted != 1 || split != 1 {
			t.Fatalf("adopted=%d split=%d, want 1,1", adopted, split)
		}
		if _, stillLegacy := state.Proxies[addr]; stillLegacy {
			t.Errorf("legacy key %q still present after split adoption", addr)
		}
		got, ok := state.Proxies[wantWinner]
		if !ok {
			t.Fatalf("expected winner key %q missing (input order %v)", wantWinner, order)
		}
		if got.ID != 99 || got.AuthFailures != 12 {
			t.Errorf("winner lost history: %+v", got)
		}
		if _, exists := state.Proxies[userB.Key()]; exists {
			t.Error("loser identity must not get a fabricated entry from adoption — it gets one later via the normal resolveProxyID path, with a fresh ID")
		}
	}
}

// TestAdoptLegacyProxyState_UnclaimedLegacyEntryIsLeftAlone: a legacy
// address with no current claimant (proxy removed from config, or simply
// not part of this particular reload's desired set) must not be touched —
// deleting it here would be indistinguishable from silently dropping
// history for a proxy that is still configured elsewhere.
func TestAdoptLegacyProxyState_UnclaimedLegacyEntryIsLeftAlone(t *testing.T) {
	const addr = "5.6.7.8:1080"
	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: {ID: 5}}}

	adopted, split := adoptLegacyProxyState(state, nil)

	if adopted != 0 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 0,0 with no desired settings at all", adopted, split)
	}
	if got, ok := state.Proxies[addr]; !ok || got.ID != 5 {
		t.Errorf("unclaimed legacy entry was disturbed: %+v ok=%v", got, ok)
	}
}

// TestAdoptLegacyProxyState_Idempotent: calling adoption twice with the
// same desired set must be a no-op the second time — the entry is already
// at its new key, so there is no legacy entry left at the bare address to
// find.
func TestAdoptLegacyProxyState_Idempotent(t *testing.T) {
	const addr = "1.2.3.4:1080"
	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: {ID: 42}}}
	desired := []*connect.ProxySettings{authSettings(addr, "alice", "pw1")}

	adoptLegacyProxyState(state, desired)
	beforeIDs := map[string]int{}
	for k, v := range state.Proxies {
		beforeIDs[k] = v.ID
	}

	adopted, split := adoptLegacyProxyState(state, desired)

	if adopted != 0 || split != 0 {
		t.Fatalf("second adoption pass: adopted=%d split=%d, want 0,0 (nothing legacy left to adopt)", adopted, split)
	}
	if len(state.Proxies) != len(beforeIDs) {
		t.Fatalf("second adoption pass changed the entry count: %d -> %d", len(beforeIDs), len(state.Proxies))
	}
	for k, wantID := range beforeIDs {
		if got, ok := state.Proxies[k]; !ok || got.ID != wantID {
			t.Errorf("second adoption pass changed entry %q: ID %d -> %+v (ok=%v)", k, wantID, got, ok)
		}
	}
}

// TestAdoptLegacyProxyState_PasswordOnlyChangeIsNotASplit: two settings at
// the same address+user with different passwords must be treated as ONE
// identity (a rotation), not a split — Key() already excludes password for
// exactly this reason (see ProxySettings.Key()'s doc comment).
func TestAdoptLegacyProxyState_PasswordOnlyChangeIsNotASplit(t *testing.T) {
	const addr = "1.2.3.4:1080"
	legacy := ProxyEntry{ID: 1, AuthFailures: 9}
	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: legacy}}

	desired := []*connect.ProxySettings{
		authSettings(addr, "alice", "old-pw"),
		authSettings(addr, "alice", "new-pw"),
	}
	adopted, split := adoptLegacyProxyState(state, desired)

	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0 — same user, different password is a rotation, not a split", adopted, split)
	}
	got, ok := state.Proxies[desired[0].Key()]
	if !ok || got.ID != 1 || got.AuthFailures != 9 {
		t.Errorf("history lost on a password-only change: %+v ok=%v", got, ok)
	}
}
