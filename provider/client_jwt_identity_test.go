package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func credSettings(addr, user string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}
}

func newJWTStoreForTest(t *testing.T) *clientJWTStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return newClientJWTStore(filepath.Join(t.TempDir(), "jwts.json"))
}

func entryWithClient(clientID string) clientJWTEntry {
	return clientJWTEntry{ByClientJWT: "jwt-" + clientID, ClientID: clientID, NetworkID: "net-main", MintedAt: time.Now()}
}

// The store key is the proxy identity: two accounts at one gateway must not
// share a slot, and the "direct" sentinel and unauthenticated proxies keep
// their existing keys.
func TestJWTStoreKey(t *testing.T) {
	if got := jwtStoreKey(nil); got != "direct" {
		t.Fatalf("nil settings must use the direct sentinel, got %q", got)
	}
	if got := jwtStoreKey(&connect.ProxySettings{Address: "10.0.0.1:1080"}); got != "10.0.0.1:1080" {
		t.Fatalf("an unauthenticated proxy keeps its bare address, got %q", got)
	}
	a, b := credSettings("gw.example:1080", "u1"), credSettings("gw.example:1080", "u2")
	if jwtStoreKey(a) == jwtStoreKey(b) {
		t.Fatalf("two accounts at one gateway must not share a JWT slot: %q", jwtStoreKey(a))
	}
	if jwtStoreKey(a) != a.Key() {
		t.Fatalf("credentialed proxy must key by identity, got %q want %q", jwtStoreKey(a), a.Key())
	}
}

// Revoking one account's identity (the watcher deletes its key) must leave the
// other account's saved client login alone.
func TestJWTStore_EvictingOneAccountKeepsTheOther(t *testing.T) {
	s := newJWTStoreForTest(t)
	a, b := credSettings("gw.example:1080", "u1"), credSettings("gw.example:1080", "u2")
	_ = s.Put(jwtStoreKey(a), entryWithClient("client-a"))
	_ = s.Put(jwtStoreKey(b), entryWithClient("client-b"))

	if err := s.Delete(jwtStoreKey(a)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(jwtStoreKey(b)); !ok {
		t.Fatalf("evicting account u1 must not delete account u2's saved login")
	}
}

// Legacy entries were stored under the bare address. With two accounts at one
// address exactly one (the lexicographically smallest key, the rule the other
// identity stores use) inherits it; the other mints fresh. The legacy slot is
// removed so an evicted or revoked identity can never be resurrected from it.
func TestJWTStoreAdoptLegacy_SharedGatewayOneWinnerLegacyRemoved(t *testing.T) {
	s := newJWTStoreForTest(t)
	a, b := credSettings("gw.example:1080", "u1"), credSettings("gw.example:1080", "u2")
	winner, loser := jwtStoreKey(a), jwtStoreKey(b)
	if loser < winner {
		winner, loser = loser, winner
	}
	_ = s.Put("gw.example:1080", entryWithClient("legacy-client"))

	adopted, split := s.AdoptLegacy([]*connect.ProxySettings{b, a})
	if adopted != 1 || split != 1 {
		t.Fatalf("adopted=%d split=%d, want 1 and 1", adopted, split)
	}
	if e, ok := s.Get(winner); !ok || e.ClientID != "legacy-client" {
		t.Fatalf("smallest key must inherit the legacy login, got ok=%v %+v", ok, e)
	}
	if _, ok := s.Get(loser); ok {
		t.Fatalf("the other account must NOT inherit a shared identity: it mints fresh")
	}
	if _, ok := s.Get("gw.example:1080"); ok {
		t.Fatalf("the legacy address-keyed slot must be removed after adoption")
	}
}

// One account at an address simply moves to its identity key.
func TestJWTStoreAdoptLegacy_SingleAccountMoves(t *testing.T) {
	s := newJWTStoreForTest(t)
	a := credSettings("gw.example:1080", "u1")
	_ = s.Put("gw.example:1080", entryWithClient("legacy-client"))

	adopted, split := s.AdoptLegacy([]*connect.ProxySettings{a})
	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1 and 0", adopted, split)
	}
	if e, ok := s.Get(jwtStoreKey(a)); !ok || e.ClientID != "legacy-client" {
		t.Fatalf("legacy login must move to the identity key, got ok=%v %+v", ok, e)
	}
}

// An unauthenticated proxy at the same address legitimately owns the bare
// address slot; adoption must leave it alone.
func TestJWTStoreAdoptLegacy_UnauthenticatedProxyKeepsTheBareSlot(t *testing.T) {
	s := newJWTStoreForTest(t)
	bare := &connect.ProxySettings{Network: "tcp", Address: "gw.example:1080"}
	a := credSettings("gw.example:1080", "u1")
	_ = s.Put("gw.example:1080", entryWithClient("bare-client"))

	s.AdoptLegacy([]*connect.ProxySettings{bare, a})
	if e, ok := s.Get("gw.example:1080"); !ok || e.ClientID != "bare-client" {
		t.Fatalf("the unauthenticated proxy's slot must survive, got ok=%v %+v", ok, e)
	}
	if _, ok := s.Get(jwtStoreKey(a)); ok {
		t.Fatalf("a credentialed account must not steal the unauthenticated proxy's login")
	}
}

// Adoption is idempotent and never overwrites a newer identity-keyed entry with
// the stale legacy one, but it does still drop the legacy slot.
func TestJWTStoreAdoptLegacy_IdempotentAndNeverOverwritesNewerEntry(t *testing.T) {
	s := newJWTStoreForTest(t)
	a := credSettings("gw.example:1080", "u1")
	_ = s.Put("gw.example:1080", entryWithClient("stale-legacy"))
	_ = s.Put(jwtStoreKey(a), entryWithClient("current"))

	s.AdoptLegacy([]*connect.ProxySettings{a})
	s.AdoptLegacy([]*connect.ProxySettings{a})
	if e, _ := s.Get(jwtStoreKey(a)); e.ClientID != "current" {
		t.Fatalf("a newer identity-keyed entry must never be overwritten, got %+v", e)
	}
	if _, ok := s.Get("gw.example:1080"); ok {
		t.Fatalf("the stale legacy slot must be dropped")
	}
}

// Warmth (which decides launch order and stagger) reads the same store, so it
// must look proxies up by identity: an account with its own saved login is warm,
// and its neighbour at the same gateway with none is cold.
func TestPrioritizeAndScheduleProxies_WarmthIsPerIdentity(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	restore := withGlobalStore(t, filepath.Join(t.TempDir(), ".client_jwts.json"))
	defer restore()

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	})
	warm, cold := credSettings("gw.example:1080", "u1"), credSettings("gw.example:1080", "u2")
	_ = globalClientJWTStore.Put(jwtStoreKey(warm), clientJWTEntry{ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main"})

	_, warmN, renewN, coldN := prioritizeAndScheduleProxies([]*connect.ProxySettings{warm, cold}, map[string]string{}, "net-main")
	if warmN != 1 || renewN != 0 || coldN != 1 {
		t.Fatalf("warm=%d renewable=%d cold=%d, want 1 warm (u1) and 1 cold (u2)", warmN, renewN, coldN)
	}
}
