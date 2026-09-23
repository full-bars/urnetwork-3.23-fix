package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestSlowRetryAdoptLegacy_SingleIdentityCarriesOverClock(t *testing.T) {
	s := newProxySlowRetryState()
	const addr = "1.2.3.4:1080"
	started := time.Now().Add(-5 * 24 * time.Hour)
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	desired := []*connect.ProxySettings{authSettings(addr, "alice", "pw1")}
	adopted, split := s.adoptLegacy(desired)

	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0", adopted, split)
	}
	if _, stillLegacy := s.Proxies[addr]; stillLegacy {
		t.Error("legacy slow-retry entry still present after adoption")
	}
	got, ok := s.Proxies[desired[0].Key()]
	if !ok || !got.StartedAt.Equal(started) {
		t.Fatalf("adopted entry lost its StartedAt (the 14-day drop clock): got=%+v ok=%v, want StartedAt=%v", got, ok, started)
	}
}

// TestSlowRetryAdoptLegacy_SplitLoserStartsFreshNotDropped is the
// production-safety property: the identity that does NOT win the legacy
// entry must simply have no slow-retry record at all (i.e. it is treated
// as never having failed), not inherit a near-expired drop clock or a
// stale DroppedAt from an account it has nothing to do with.
func TestSlowRetryAdoptLegacy_SplitLoserStartsFreshNotDropped(t *testing.T) {
	const addr = "dc.decodo.com:10058"
	s := newProxySlowRetryState()
	started := time.Now().Add(-13 * 24 * time.Hour) // one day from being dropped
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	userA := authSettings(addr, "sppmr4vcnj", "pw1")
	userB := authSettings(addr, "user-sppmr4vcnj-country-us-city-metro", "pw2")

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{userA, userB})
	if adopted != 1 || split != 1 {
		t.Fatalf("adopted=%d split=%d, want 1,1", adopted, split)
	}
	if _, exists := s.Proxies[userB.Key()]; exists {
		t.Error("loser identity must have no slow-retry entry at all after a split, not an inherited near-expired clock")
	}
	if got, ok := s.Proxies[userA.Key()]; !ok || !got.StartedAt.Equal(started) {
		t.Errorf("winner lost its clock: got=%+v ok=%v", got, ok)
	}
}

func TestSlowRetryAdoptLegacy_NoAuthIsANoOp(t *testing.T) {
	s := newProxySlowRetryState()
	const addr = "1.2.3.4:1080"
	started := time.Now()
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{noAuthSettings(addr)})

	if adopted != 0 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 0,0", adopted, split)
	}
	if got := s.Proxies[addr]; !got.StartedAt.Equal(started) {
		t.Errorf("no-auth entry was disturbed: %+v", got)
	}
}
