package gossip

import "testing"

func TestGossipStreamLimitKeepsOtherPeerAvailable(t *testing.T) {
	const limit = 8
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		settings.NegotiationTimeout = testHeldNegotiationTimeout
		settings.MaxInboundStreamCountPerConn = limit
	})
	first := target.dial(newTestKey(t))
	second := target.dial(newTestKey(t))
	if !openHeldStream(t, target.ctx, second) {
		t.Fatal("the second peer could not open its first stream")
	}
	held := openHeldStreams(t, target.ctx, first, 2*limit)
	if held == 0 || held > limit {
		t.Fatalf("first peer held %d streams with a limit of %d", held, limit)
	}
	if !openHeldStream(t, target.ctx, second) {
		t.Fatal("the second peer could not open another stream")
	}
}
