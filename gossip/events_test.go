package gossip

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventRingWrapAndBounds(t *testing.T) {
	var r EventRing
	for i := 0; i < EventCapacity+10; i++ {
		if !r.Record(Event{Time: time.Unix(int64(i), 0), Kind: strings.Repeat("k", 200), Peer: strings.Repeat("p", 200), Direction: strings.Repeat("d", 200), Reason: strings.Repeat("r", 200)}) {
			t.Fatal("uncontended record dropped")
		}
	}
	events := r.Snapshot()
	if len(events) != EventCapacity || events[0].Time.Unix() != 10 || events[len(events)-1].Time.Unix() != EventCapacity+9 {
		t.Fatalf("unexpected ring contents: length=%d first=%v last=%v", len(events), events[0].Time, events[len(events)-1].Time)
	}
	e := events[0]
	if len(e.Kind) != 16 || len(e.Peer) != 8 || len(e.Direction) != 16 || len(e.Reason) != 64 {
		t.Fatalf("fields not bounded: %+v", e)
	}
	events[0].Kind = "changed"
	if r.Snapshot()[0].Kind == "changed" {
		t.Fatal("snapshot aliases ring")
	}
}

func TestEventRingDropsWhenBusy(t *testing.T) {
	var r EventRing
	r.mu.Lock()
	if r.Record(Event{Kind: "peer"}) {
		t.Fatal("busy ring accepted an event")
	}
	r.mu.Unlock()
	if len(r.Snapshot()) != 0 {
		t.Fatal("busy ring changed")
	}
}

func TestEventRingConcurrent(t *testing.T) {
	var r EventRing
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.Record(Event{Kind: "peer"})
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if len(r.Snapshot()) > EventCapacity {
		t.Fatal("ring exceeded its capacity")
	}
}
