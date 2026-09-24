package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The graph anchors both series on one number, HistorySeq. If a tick lands
// between reading that number and reading the history it labels, the anchor is
// one sample stale and every completed graph column shifts by one. Below the
// ring size the invariant is exact: the snapshot holds as many samples as have
// ever been recorded, in BOTH series.
func TestSnapshotHistorySeqMatchesItsHistoryUnderConcurrentTicks(t *testing.T) {
	var n atomic.Uint64
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 1}}
	src := env.sources()
	// The counters grow every call so every tick records a sample; the clock
	// advances a second a tick, read only by the ticking goroutine.
	src.billable = func() map[string]uint64 { return map[string]uint64{"p": n.Load() * 10} }
	src.traffic = func() map[string]uint64 { return map[string]uint64{"p": n.Load() * 20} }
	var tickNow atomic.Int64
	src.now = func() time.Time { return snapT0.Add(time.Duration(tickNow.Load()) * time.Second) }
	c := newNodeSnapshotCollector(src)

	const ticks = 550 // stay under the 600 sample ring so length == seq exactly
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= ticks; i++ {
			n.Store(uint64(i))
			tickNow.Store(int64(i))
			c.tick()
			if i%16 == 0 {
				time.Sleep(50 * time.Microsecond) // let the reader interleave
			}
		}
		close(stop)
	}()

	builds, bad := 0, 0
	for {
		select {
		case <-stop:
			wg.Wait()
			if builds < 200 {
				t.Fatalf("only %d snapshots were built while ticking; the test is not exercising the race", builds)
			}
			if bad > 0 {
				t.Fatalf("%d of %d snapshots had a HistorySeq that disagrees with the history it labels", bad, builds)
			}
			return
		default:
		}
		snap := c.build(snapT0.Add(time.Hour))
		builds++
		seq := int(snap.Rate.HistorySeq)
		if len(snap.Rate.HistoryBps) != seq || (snap.Traffic != nil && len(snap.Traffic.TotalHistoryBps) != seq) {
			bad++
			if bad <= 3 {
				t.Logf("seq=%d billable=%d total=%d", seq, len(snap.Rate.HistoryBps), len(snap.Traffic.TotalHistoryBps))
			}
		}
	}
}
