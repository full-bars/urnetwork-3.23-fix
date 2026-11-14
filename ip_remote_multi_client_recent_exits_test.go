package connect

import (
	"testing"
	"time"
)

func TestRecentExitMemoryLifecycle(t *testing.T) {
	start := time.Unix(100, 0)
	timeout := time.Minute
	a := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start}
	b := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(time.Second)}
	c := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(2 * time.Second)}
	var memory recentExitMemory
	memory.Remember(b, 2, timeout)
	memory.Remember(a, 2, timeout)
	evicted, expired := memory.Remember(c, 2, timeout)
	if len(evicted) != 1 || evicted[0].destination != a.destination || len(expired) != 0 {
		t.Fatalf("evicted=%v expired=%v", evicted, expired)
	}
	taken, expired, generation := memory.Take(start.Add(3*time.Second), timeout)
	if len(taken) != 2 || taken[0].destination != b.destination || taken[1].destination != c.destination || len(expired) != 0 {
		t.Fatalf("taken=%v expired=%v", taken, expired)
	}
	// A fresh loss supersedes an older entry restored by an in-flight lookup.
	newB := b
	newB.lostTime = start.Add(4 * time.Second)
	memory.Remember(newB, 2, timeout)
	memory.Restore(taken, generation)
	taken, _, generation = memory.Take(start.Add(5*time.Second), timeout)
	if len(taken) != 2 || taken[1].destination != b.destination || !taken[1].lostTime.Equal(newB.lostTime) {
		t.Fatalf("restored=%v", taken)
	}
	memory.ForgetProvider(b.destination.Tail())
	if memory.Current(generation) {
		t.Fatal("forget did not invalidate generation")
	}
	memory.Restore(taken, generation)
	if taken, _, _ = memory.Take(start, timeout); len(taken) != 0 {
		t.Fatalf("stale restore=%v", taken)
	}
}

func TestRecentExitMemoryExpirationAndForget(t *testing.T) {
	start := time.Unix(100, 0)
	a := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start}
	b := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(time.Minute)}
	var memory recentExitMemory
	memory.Remember(a, 1, time.Minute)
	evicted, expired := memory.Remember(b, 1, time.Minute)
	if len(evicted) != 0 || len(expired) != 1 || expired[0].destination != a.destination {
		t.Fatalf("evicted=%v expired=%v", evicted, expired)
	}
	taken, expired, _ := memory.Take(b.lostTime.Add(time.Minute), time.Minute)
	if len(taken) != 0 || len(expired) != 1 {
		t.Fatalf("taken=%v expired=%v", taken, expired)
	}
	a.lostTime = b.lostTime
	memory.Remember(a, 2, time.Minute)
	memory.Remember(b, 2, time.Minute)
	if !memory.ForgetProvider(a.destination.Tail()) || memory.ForgetProvider(a.destination.Tail()) {
		t.Fatal("provider forget result")
	}
	if memory.Forget() != 1 {
		t.Fatal("forget count")
	}
	memory.Remember(a, 0, time.Minute)
	if taken, _, _ = memory.Take(start, time.Minute); len(taken) != 0 {
		t.Fatal("zero cap retained entry")
	}
	memory.Remember(a, 1, time.Minute)
	if taken, expired, _ = memory.Take(start, 0); len(taken) != 0 || len(expired) != 1 {
		t.Fatal("disabled timeout retained entry")
	}
}

func TestRecentExitMemoryCapacity(t *testing.T) {
	if got := recentExitMemoryMaxCount(WindowSizeSettings{WindowSizeHardMax: 2, WindowSizeMax: 4}); got != 2 {
		t.Fatalf("hard max: %d", got)
	}
	if got := recentExitMemoryMaxCount(WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 3, FixedWindowSize: 4}); got != 4 {
		t.Fatalf("max sizes: %d", got)
	}
}
