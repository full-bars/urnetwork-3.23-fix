package main

import (
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyUDPScanRingBasic(t *testing.T) {
	r := newProxyUDPScanRing(4)
	if events := r.Snapshot(); len(events) != 0 {
		t.Fatalf("expected empty snapshot, got %d", len(events))
	}

	t0 := time.Unix(100, 0)
	t1 := time.Unix(101, 0)
	t2 := time.Unix(102, 0)

	r.Record(1, "ok", t0)
	r.Record(2, "no_relay", t1)
	r.Record(3, "mismatch", t2)

	events := r.Snapshot()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Index != 1 || events[0].Status != "ok" || !events[0].At.Equal(t0) {
		t.Errorf("event 0 mismatch: %+v", events[0])
	}
	if events[1].Index != 2 || events[1].Status != "no_relay" || !events[1].At.Equal(t1) {
		t.Errorf("event 1 mismatch: %+v", events[1])
	}
	if events[2].Index != 3 || events[2].Status != "mismatch" || !events[2].At.Equal(t2) {
		t.Errorf("event 2 mismatch: %+v", events[2])
	}
}

func TestProxyUDPScanRingOverflowWraps(t *testing.T) {
	r := newProxyUDPScanRing(3)
	tBase := time.Unix(1000, 0)

	for i := 1; i <= 5; i++ {
		r.Record(i, "ok", tBase.Add(time.Duration(i)*time.Second))
	}

	events := r.Snapshot()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	// Ring kept the last 3 events: 3, 4, 5
	for i, wantIdx := range []int{3, 4, 5} {
		if events[i].Index != wantIdx {
			t.Errorf("event %d index = %d, want %d", i, events[i].Index, wantIdx)
		}
	}
}

func TestProxyUDPScanRingConcurrent(t *testing.T) {
	r := newProxyUDPScanRing(64)
	var wg sync.WaitGroup
	now := time.Now()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				r.Record(id*100+i, "ok", now)
				_ = r.Snapshot()
			}
		}(worker)
	}
	wg.Wait()

	events := r.Snapshot()
	if len(events) > 64 {
		t.Fatalf("snapshot length %d exceeded capacity 64", len(events))
	}
}

func TestProxyUDPStoreRecordEvent(t *testing.T) {
	store := newProxyUDPCheckStore(nil)
	if events := store.RecentEvents(); len(events) != 0 {
		t.Fatalf("expected empty store events, got %d", len(events))
	}

	now := time.Unix(500, 0)
	settings := &connect.ProxySettings{
		Index:   7,
		Address: "192.0.2.1:1080",
	}

	store.recordEvent(settings, proxyUDPResult{Status: proxyUDPOK}, now)
	store.recordEvent(settings, proxyUDPResult{Status: proxyUDPNoRelay}, now.Add(time.Second))
	store.recordEvent(settings, proxyUDPResult{Status: proxyUDPExitMismatch}, now.Add(2*time.Second))
	store.recordEvent(settings, proxyUDPResult{Status: proxyUDPUnknown}, now.Add(3*time.Second))

	events := store.RecentEvents()
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	wantStatuses := []string{"ok", "no_relay", "mismatch", "unknown"}
	for i, want := range wantStatuses {
		if events[i].Status != want {
			t.Errorf("event %d status = %q, want %q", i, events[i].Status, want)
		}
		if events[i].Index != 7 {
			t.Errorf("event %d index = %d, want 7", i, events[i].Index)
		}
	}
}
