package gossip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/urnetwork/connect"
)

type dialStatsHost struct {
	host.Host
	err   error
	store peerstore.Peerstore
}

func (h *dialStatsHost) Peerstore() peerstore.Peerstore { return h.store }

func (h *dialStatsHost) Connect(context.Context, peer.AddrInfo) error { return h.err }

func TestDialFailureClassSwarmErrors(t *testing.T) {
	for _, tc := range []struct {
		class string
		err   error
	}{
		{"backoff", swarm.ErrDialBackoff},
		{"no_addrs", swarm.ErrNoGoodAddresses},
		{"no_addrs", swarm.ErrNoAddresses},
		{"gated", swarm.ErrGaterDisallowedConnection},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("wrapped: %w", tc.err), &swarm.DialError{Cause: tc.err}, &swarm.DialError{DialErrors: []swarm.TransportError{{Cause: tc.err}}}} {
			if got := dialFailureClass(err); got != tc.class {
				t.Errorf("class of %v = %s, want %s", err, got, tc.class)
			}
		}
	}
}

func TestGossipDialStats(t *testing.T) {
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	h := &dialStatsHost{store: store}
	n := &Node{ctx: context.Background(), host: h, settings: &NodeSettings{DialTimeout: time.Second}, log: connect.NewNoopLogger()}
	cases := []error{
		nil,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		context.Canceled,
		errors.New("unexpected"),
		swarm.ErrDialBackoff,
		swarm.ErrNoGoodAddresses,
		swarm.ErrGaterDisallowedConnection,
	}
	for _, err := range cases {
		h.err = err
		n.connectPeer(peer.AddrInfo{})
	}
	want := (DialStats{Attempts: 8, Timeout: 1, Refused: 1, Canceled: 1, Backoff: 1, NoAddrs: 1, Gated: 1, Other: 1})
	if got := n.DialStats(); got != want {
		t.Fatalf("dial stats = %+v, want %+v", got, want)
	}
}
