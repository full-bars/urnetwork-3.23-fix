package main

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// waitSignal waits for one value on ch and fails the test instead of hanging.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestUDPProbeRunnerUsesSuppliedTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ps := &connect.ProxySettings{Address: "example:1080"}
	calls := 0
	store := newProxyUDPCheckStore(func(_ context.Context, _ *connect.ProxySettings) proxyUDPResult {
		calls++
		return proxyUDPResult{Status: proxyUDPOK}
	})
	runUDPProbeTick(context.Background(), store, []*connect.ProxySettings{ps}, now, time.Hour, time.Hour, 0, true)
	runUDPProbeTick(context.Background(), store, []*connect.ProxySettings{ps}, now.Add(25*time.Hour), time.Hour, time.Hour, 0, true)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if got := store.entries[ps.Key()].at; !got.Equal(now.Add(25 * time.Hour)) {
		t.Fatalf("timestamp = %v, want supplied time", got)
	}
}

func TestUDPProbeRunnerBoundAndResults(t *testing.T) {
	var active, peak, count atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	store := newProxyUDPCheckStore(func(ctx context.Context, s *connect.ProxySettings) proxyUDPResult {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		count.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return proxyUDPResult{Status: proxyUDPOK}
	})
	online := make([]*connect.ProxySettings, 4)
	for i := range online {
		online[i] = &connect.ProxySettings{Address: fmt.Sprintf("127.0.0.1:%d", i+1)}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUDPProbeTick(context.Background(), store, online, time.Now(), time.Minute, time.Minute, 0, true)
	}()
	waitSignal(t, entered, "the first probe")
	waitSignal(t, entered, "the second probe")
	close(release)
	// The two later probes fit in the entered buffer, so no drain is needed.
	waitSignal(t, done, "the runner to finish")
	if peak.Load() != 2 || active.Load() != 0 || count.Load() != 4 {
		t.Fatalf("peak=%d active=%d count=%d", peak.Load(), active.Load(), count.Load())
	}
	if got := store.coverage([]string{online[0].Key(), online[1].Key(), online[2].Key(), online[3].Key()}, time.Now()); got.OK != 4 {
		t.Fatalf("coverage: %+v", got)
	}
}

func TestUDPProbeRunnerSlotAndPressureBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		slots    int
		pressure float64
	}{
		{"slot", 1, 0}, {"pressure", 8, .99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var active, peak, count atomic.Int32
			store := newProxyUDPCheckStore(func(_ context.Context, _ *connect.ProxySettings) proxyUDPResult {
				n := active.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				count.Add(1)
				runtime.Gosched()
				active.Add(-1)
				return proxyUDPResult{Status: proxyUDPOK}
			})
			store.slots = make(chan struct{}, tc.slots)
			online := []*connect.ProxySettings{{Address: "a:1"}, {Address: "b:2"}, {Address: "c:3"}}
			runUDPProbeTick(context.Background(), store, online, time.Now(), time.Minute, time.Minute, tc.pressure, true)
			if count.Load() != 3 || peak.Load() != 1 || active.Load() != 0 {
				t.Fatalf("count=%d peak=%d active=%d", count.Load(), peak.Load(), active.Load())
			}
		})
	}
}

func TestUDPProbeRunnerSkipsAndCancels(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	store := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return proxyUDPResult{Status: proxyUDPNoRelay}
	})
	online := []*connect.ProxySettings{{Address: "127.0.0.1:1"}, {Address: "127.0.0.1:2"}, {Address: "127.0.0.1:3"}}
	now := time.Now()
	runUDPProbeTick(context.Background(), store, online, now, time.Minute, time.Minute, udpProbeSkipPressure, true)
	runUDPProbeTick(context.Background(), store, online, now, time.Minute, time.Minute, 0, false)
	if calls.Load() != 0 {
		t.Fatal("disabled or pressured runner probed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runUDPProbeTick(ctx, store, online, now, time.Minute, time.Minute, 0, true) }()
	waitSignal(t, entered, "the first probe")
	waitSignal(t, entered, "the second probe")
	cancel()
	waitSignal(t, done, "the runner to stop after cancel")
	if calls.Load() != 2 || store.size() != 0 {
		t.Fatalf("calls=%d cached=%d", calls.Load(), store.size())
	}
}

// A batch that outlasts its tick is cut off at the tick interval, so a slow
// batch cannot hold the reaper or the grader past their own interval.
func TestUDPProbeRunnerStopsAtTheTickInterval(t *testing.T) {
	store := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		<-ctx.Done() // a probe that never answers on its own
		return proxyUDPResult{Status: proxyUDPNoRelay}
	})
	online := []*connect.ProxySettings{{Address: "127.0.0.1:1"}, {Address: "127.0.0.1:2"}, {Address: "127.0.0.1:3"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUDPProbeTick(context.Background(), store, online, time.Now(), time.Minute, 50*time.Millisecond, 0, true)
	}()
	waitSignal(t, done, "the tick to stop at its interval")
	if store.size() != 0 {
		t.Fatalf("interrupted checks were remembered: %d", store.size())
	}
}
