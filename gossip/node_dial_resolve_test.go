package gossip

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/urnetwork/connect"
)

// An address the peerstore holds for a peer, such as a name the peer announced
// over identify, must be gone before a dial. The swarm resolves names before
// the gater can refuse them, so a name left in the peerstore costs a lookup.
func TestGossipConnectPeerDropsAddressesThatWereNotChosen(t *testing.T) {
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	h := &dialStatsHost{store: store}
	n := &Node{ctx: context.Background(), host: h, settings: &NodeSettings{DialTimeout: time.Second}, log: connect.NewNoopLogger()}

	peerId, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	unchosen := newTestMultiaddr(t, "/dns4/unchosen.invalid/tcp/443/ws")
	store.AddAddr(peerId, unchosen, peerstore.PermanentAddrTTL)
	if !slices.ContainsFunc(store.Addrs(peerId), unchosen.Equal) {
		t.Fatal("the test address was not stored")
	}

	n.connectPeer(peer.AddrInfo{ID: peerId, Addrs: []ma.Multiaddr{newTestMultiaddr(t, "/ip4/127.0.0.1/tcp/1/ws")}})

	if slices.ContainsFunc(store.Addrs(peerId), unchosen.Equal) {
		t.Fatal("an address the round did not choose was still in the peerstore when the node dialed")
	}
}
