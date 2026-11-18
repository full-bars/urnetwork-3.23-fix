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
	before := runtime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUDPProbeTick(context.Background(), store, online, time.Now(), time.Minute, time.Minute, 0, true)
	}()
	<-entered
	<-entered
	close(release)
	// Drain further notifications without constraining workers.
	for count.Load() < int32(len(online)) {
		select {
		case <-entered:
		case <-done:
			t.Fatal("runner stopped early")
		}
	}
	<-done
	if peak.Load() != 2 || active.Load() != 0 || count.Load() != 4 {
		t.Fatalf("peak=%d active=%d count=%d", peak.Load(), active.Load(), count.Load())
	}
	if got := store.coverage([]string{online[0].Key(), online[1].Key(), online[2].Key(), online[3].Key()}, time.Now()); got.OK != 4 {
		t.Fatalf("coverage: %+v", got)
	}
	for i := 0; i < 1000 && runtime.NumGoroutine() > before; i++ {
		runtime.Gosched()
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines before=%d after=%d", before, after)
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
	<-entered
	<-entered
	cancel()
	<-done
	if calls.Load() != 2 || store.size() != 0 {
		t.Fatalf("calls=%d cached=%d", calls.Load(), store.size())
	}
}
