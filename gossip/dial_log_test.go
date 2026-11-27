package gossip

import (
	"fmt"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

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
