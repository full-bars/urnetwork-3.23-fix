package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A direct P2P stream A <-> C, the only stream shape production builds: the
// server returns no intermediaries, so each endpoint's adjacent hop is the
// other endpoint. Each receive lane is a real P2P receive transport bound to
// the other end, and the link carries bytes unchanged. Drop mode delivers and
// acks every frame and drops none, because every frame names the bound peer.
func TestP2PDirectStreamWorksInDropMode(t *testing.T) {
	setSourcePolicy(t, "drop", true)
	const count = 8

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	aId := NewId()
	cId := NewId()
	streamId := NewId()
	a, aLane := newP2PTestEndpoint(t, ctx, aId, cId, streamId)
	c, cLane := newP2PTestEndpoint(t, ctx, cId, aId, streamId)
	a.client.ContractManager().AddNoContractPeer(cId)
	c.client.ContractManager().AddNoContractPeer(aId)

	// The direct link: A's frames to C's lane, C's acks to A's lane.
	runMultihopRelay(t, ctx, a.out, cLane, streamId)
	runMultihopRelay(t, ctx, c.out, aLane, streamId)

	acks := make(chan error, count)
	for i := 0; i < count; i += 1 {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: fmt.Sprintf("direct-%d", i)}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !a.client.SendWithTimeout(frame, DestinationId(cId), func(err error) { acks <- err }, 5*time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("send %d not admitted", i)
		}
	}

	delivered := 0
	acked := 0
	deadline := time.After(10 * time.Second)
	for delivered < count || acked < count {
		select {
		case <-c.delivered:
			delivered += 1
		case err := <-acks:
			if err != nil {
				t.Fatalf("ack error: %v", err)
			}
			acked += 1
		case <-deadline:
			t.Fatalf("drop mode direct stream: delivered %d/%d, acked %d/%d; C stats %+v; A stats %+v",
				delivered, count, acked, count, c.client.ReceiveStats(), a.client.ReceiveStats())
		}
	}

	for name, endpoint := range map[string]*multihopTestEndpoint{"A": a, "C": c} {
		if stats := endpoint.client.ReceiveStats(); stats.SourceMismatchCount != 0 || stats.SourceMismatchDroppedCount != 0 || stats.ForgedSourceDropCount != 0 {
			t.Fatalf("%s dropped or counted frames on a direct stream: %+v", name, stats)
		}
	}
}
