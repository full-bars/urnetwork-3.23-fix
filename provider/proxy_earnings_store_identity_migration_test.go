package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestEarningsAdoptLegacy_SingleIdentityCarriesOver(t *testing.T) {
	s := newProxyEarningsStore("")
	const addr = "1.2.3.4:1080"
	s.entries[addr] = &proxyEarningsEntry{Score: 500, Updated: time.Now()}

	desired := []*connect.ProxySettings{authSettings(addr, "alice", "pw1")}
	adopted, split := s.adoptLegacy(desired)

	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0", adopted, split)
	}
	if _, stillLegacy := s.entries[addr]; stillLegacy {
		t.Error("legacy earnings entry still present after adoption")
	}
	got, ok := s.entries[desired[0].Key()]
	if !ok || got.Score != 500 {
		t.Fatalf("adopted entry lost its score: %+v ok=%v", got, ok)
	}
}

func TestEarningsAdoptLegacy_NoAuthIsANoOp(t *testing.T) {
	s := newProxyEarningsStore("")
	const addr = "1.2.3.4:1080"
	s.entries[addr] = &proxyEarningsEntry{Score: 100}

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{noAuthSettings(addr)})

	if adopted != 0 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 0,0", adopted, split)
	}
	if got := s.entries[addr]; got.Score != 100 {
		t.Errorf("no-auth entry was disturbed: %+v", got)
	}
}

// TestEarningsAdoptLegacy_SplitNeverDuplicatesMoney is the money-safety
// property: a legacy earnings score must go to exactly one identity on a
// split, never be duplicated (double-counted) or dropped (zeroed) for the
// winner, and the loser must start at zero, never inherit any of it.
func TestEarningsAdoptLegacy_SplitNeverDuplicatesMoney(t *testing.T) {
	s := newProxyEarningsStore("")
	const addr = "dc.decodo.com:10058"
	s.entries[addr] = &proxyEarningsEntry{Score: 123456}

	userA := authSettings(addr, "sppmr4vcnj", "pw1")
	userB := authSettings(addr, "user-sppmr4vcnj-country-us-city-metro", "pw2")
	wantWinner := userA.Key()

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{userA, userB})
	if adopted != 1 || split != 1 {
		t.Fatalf("adopted=%d split=%d, want 1,1", adopted, split)
	}

	total := 0.0
	if e, ok := s.entries[wantWinner]; ok {
		total += e.Score
	}
	if e, ok := s.entries[userB.Key()]; ok {
		total += e.Score
		t.Errorf("loser identity got a fabricated earnings entry: %+v", e)
	}
	if total != 123456 {
		t.Errorf("total earnings changed across the split: got %v, want 123456 (money must not be duplicated or dropped)", total)
	}
}

func TestEarningsAdoptLegacy_PrevCumNeverTouched(t *testing.T) {
	s := newProxyEarningsStore("")
	const addr = "1.2.3.4:1080"
	s.entries[addr] = &proxyEarningsEntry{Score: 10}
	s.prevCum[addr] = 999 // should be impossible in practice (never persisted) but must survive untouched if present

	s.adoptLegacy([]*connect.ProxySettings{authSettings(addr, "alice", "pw")})

	if got, ok := s.prevCum[addr]; !ok || got != 999 {
		t.Errorf("prevCum[%q] was touched by adoptLegacy: got=%v ok=%v, want untouched at 999", addr, got, ok)
	}
}

// TestEarningsObserve_CreditsIdentityKeys locks down the post-migration
// ingest path: bandwidth observed under an identity key must be credited to
// that identity key — never collapsed to the bare address, which would
// recreate the legacy-key collision and leave the identity's score at zero.
// Deterministic: fixed keys and counters, no timing dependence beyond the
// same `now` for both calls.
func TestEarningsObserve_CreditsIdentityKeys(t *testing.T) {
	s := newProxyEarningsStore("")
	settings := authSettings("dc.decodo.com:10058", "sppmr4vcnj", "pw")
	key := settings.Key()

	now := time.Now()
	snapshot := map[string]*connect.ProxyBandwidth{key: bwWith(0)}
	s.Observe(snapshot, now) // baseline only
	snapshot[key] = bwWith(500)
	s.Observe(snapshot, now)

	if got := s.Score(key, now); got <= 0 {
		t.Fatalf("identity-keyed credit not visible via Score: %v", got)
	}
	if _, legacy := s.entries[settings.Address]; legacy {
		t.Fatal("Observe recreated a legacy bare-address entry — identity credits must stay identity-keyed")
	}
	if _, ok := s.entries[key]; !ok {
		t.Fatal("no entry under the identity key after Observe")
	}
}

// TestEarningsAdoptObserveAdopt_PreservesCumulativeScore is the regression
// guard for the adopt->observe->adopt cycle that used to destroy money
// history: Observe credited the bare address, and the next adoption
// replaced the identity entry with that fragment instead of accumulating.
func TestEarningsAdoptObserveAdopt_PreservesCumulativeScore(t *testing.T) {
	s := newProxyEarningsStore("")
	settings := authSettings("dc.decodo.com:10058", "sppmr4vcnj", "pw")
	key := settings.Key()
	addr := settings.Address
	now := time.Now()

	// Legacy state on disk: 100 bytes earned under the bare address.
	s.entries[addr] = &proxyEarningsEntry{Score: 100, Updated: now}

	// Startup adoption moves it to the identity key.
	s.adoptLegacy([]*connect.ProxySettings{settings})

	// Post-migration ingest credits the identity key directly.
	snapshot := map[string]*connect.ProxyBandwidth{key: bwWith(1000)}
	s.Observe(snapshot, now) // baseline at 1000 (fresh counter: no credit yet)
	snapshot[key] = bwWith(1100)
	s.Observe(snapshot, now) // +100 earned

	// A later reload's adoption must be a no-op for this proxy (no legacy
	// key was recreated) and must not disturb the accumulated score.
	adopted, _ := s.adoptLegacy([]*connect.ProxySettings{settings})
	if adopted != 0 {
		t.Fatalf("adoptLegacy re-adopted %d entries after identity-keyed ingest, want 0", adopted)
	}
	if got := s.Score(key, now); got < 199 {
		t.Fatalf("cumulative earnings lost across adopt->observe->adopt: score=%v want ~200", got)
	}
}

// TestEarningsAdoptLegacy_MergesWhenWinnerExists guards the additive merge:
// when a legacy entry and its identity-keyed entry coexist (disjoint
// earning periods), adoption must SUM them, never replace. Both entries
// share the same Updated timestamp so decay is exactly 1 and the expected
// total is exact.
func TestEarningsAdoptLegacy_MergesWhenWinnerExists(t *testing.T) {
	s := newProxyEarningsStore("")
	settings := authSettings("dc.decodo.com:10058", "sppmr4vcnj", "pw")
	key := settings.Key()
	addr := settings.Address
	now := time.Now()

	s.entries[addr] = &proxyEarningsEntry{Score: 100, Updated: now}
	s.entries[key] = &proxyEarningsEntry{Score: 50, Updated: now}

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{settings})
	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0", adopted, split)
	}
	if _, stillLegacy := s.entries[addr]; stillLegacy {
		t.Error("legacy earnings entry still present after merge")
	}
	merged, ok := s.entries[key]
	if !ok || merged.Score != 150 {
		t.Fatalf("merged entry = %+v ok=%v, want score 150 (100 legacy + 50 existing)", merged, ok)
	}
}
