package gossip

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

func TestSelectExtenderDialsBackoff(t *testing.T) {
	addr := ma.StringCast("/ip4/192.0.2.1/tcp/1234")
	a := peer.AddrInfo{ID: peer.ID("a"), Addrs: []ma.Multiaddr{addr}}
	b := peer.AddrInfo{ID: peer.ID("b"), Addrs: []ma.Multiaddr{addr}}
	now := time.Unix(100, 0)
	until := now.Add(5 * time.Second)
	backoff := func(id peer.ID, _ ma.Multiaddr) bool { return id == a.ID && now.Before(until) }
	selectDials := func() ([]peer.AddrInfo, bool) {
		return selectExtenderDials([]peer.AddrInfo{a, b}, 1, func(peer.ID) bool { return false }, func(info peer.AddrInfo) bool { return allAddrsBackedOff(info, backoff) })
	}
	got, skipped := selectDials()
	if !skipped || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("inside window: %v %v", got, skipped)
	}
	now = until
	got, skipped = selectDials()
	if skipped || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("after window: %v %v", got, skipped)
	}
	now = until.Add(-time.Second)
	until = time.Time{}
	got, skipped = selectDials()
	if skipped || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("after clear: %v %v", got, skipped)
	}
}
