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
	"github.com/urnetwork/connect"
)

type dialStatsHost struct {
	host.Host
	err error
}

func (h *dialStatsHost) Connect(context.Context, peer.AddrInfo) error { return h.err }

func TestGossipDialStats(t *testing.T) {
	h := &dialStatsHost{}
	n := &Node{ctx: context.Background(), host: h, settings: &NodeSettings{DialTimeout: time.Second}, log: connect.NewNoopLogger()}
	cases := []error{
		nil,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		context.Canceled,
		errors.New("unexpected"),
	}
	for _, err := range cases {
		h.err = err
		n.connectPeer(peer.AddrInfo{})
	}
	want := (DialStats{Attempts: 5, Timeout: 1, Refused: 1, Canceled: 1, Other: 1})
	if got := n.DialStats(); got != want {
		t.Fatalf("dial stats = %+v, want %+v", got, want)
	}
}
