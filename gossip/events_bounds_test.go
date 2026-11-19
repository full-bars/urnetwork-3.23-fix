package gossip

import (
	"testing"
	"time"
)

func TestEventPeerLabelsAndCapFlood(t *testing.T) {
	var r EventRing
	now := time.Unix(100, 0)
	r.Record(Event{Kind: "peer", Peer: "12D3KooWabcdefgh"})
	r.Record(Event{Kind: "peer", Peer: "12D3KooW12345678"})
	for _, reason := range []string{"subscriber_cap", "accept_queue_full", "resource_limit"} {
		for i := 0; i < 2000; i++ {
			r.RecordCap(Event{Time: now, Kind: "cap", Reason: reason})
		}
	}
	events := r.Snapshot()
	if len(events) != 5 || events[0].Peer != "abcdefgh" || events[1].Peer != "12345678" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if !r.RecordCap(Event{Time: now.Add(time.Minute), Kind: "cap", Reason: "subscriber_cap"}) {
		t.Fatal("cap event after interval was dropped")
	}
}
