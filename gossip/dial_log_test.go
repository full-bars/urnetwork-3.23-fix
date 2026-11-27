package gossip

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/urnetwork/connect"
)

type captureDialLogger struct {
	connect.Logger
	lines []string
}

func (l *captureDialLogger) Infof(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func TestConnectPeerLogsFailureTransitions(t *testing.T) {
	store, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := &captureDialLogger{Logger: connect.NewNoopLogger()}
	h := &dialStatsHost{store: store}
	n := &Node{ctx: context.Background(), host: h, settings: &NodeSettings{DialTimeout: time.Second}, log: logger}
	id := peer.AddrInfo{ID: peer.ID("peer")}
	for _, err := range []error{swarm.ErrDialBackoff, swarm.ErrDialBackoff, swarm.ErrGaterDisallowedConnection, nil, swarm.ErrGaterDisallowedConnection} {
		h.err = err
		n.connectPeer(id)
	}
	if len(logger.lines) != 3 {
		t.Fatalf("lines = %q", logger.lines)
	}
	for i, text := range []string{"class=backoff", "class=gated", "class=gated"} {
		if !strings.Contains(logger.lines[i], text) {
			t.Fatalf("line %d = %q", i, logger.lines[i])
		}
	}
}

func TestDialClassTransitions(t *testing.T) {
	n := &Node{}
	id := peer.ID("peer")
	for _, tc := range []struct {
		class string
		first bool
	}{
		{"backoff", true}, {"backoff", false}, {"gated", true}, {"", false}, {"gated", true},
	} {
		if got := n.noteDialClass(id, tc.class); got != tc.first {
			t.Fatalf("class %q: got %v, want %v", tc.class, got, tc.first)
		}
	}
	for i := 0; i < 2000; i++ {
		n.noteDialClass(peer.ID(fmt.Sprintf("peer-%d", i)), "other")
	}
	if len(n.dialFailureClasses) > 1024 {
		t.Fatalf("unbounded entries: %d", len(n.dialFailureClasses))
	}
}
