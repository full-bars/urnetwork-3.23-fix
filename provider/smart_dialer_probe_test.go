package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// probeFrames classifies one goroutine stack block from runtime.Stack. A probe
// loop is started as `go connect.HandleError(func(){...})`, so a goroutine that
// has been created but not scheduled yet shows only the generated wrapper
// (startSmartDialerProbes.gowrap1) — never startSmartDialerProbes.func1, which
// appears only once the closure is executing. Once scheduled inside the closure
// the block shows func1, and parked in the loop it shows runSmartDialerProbes.
// Missing the wrapper made a freshly created probe goroutine invisible.
func probeFrames(block string) (probe bool, inLoop bool) {
	inLoop = strings.Contains(block, "runSmartDialerProbes(")
	probe = inLoop ||
		strings.Contains(block, "startSmartDialerProbes.gowrap1(") ||
		strings.Contains(block, "startSmartDialerProbes.func1(")
	return probe, inLoop
}

// probeGoroutines counts the goroutines that belong to a smart dialer probe
// loop; inLoop says how many of them have entered the loop.
func probeGoroutines() (total int, inLoop int) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, block := range strings.Split(string(buf), "\n\n") {
		probe, looping := probeFrames(block)
		if !probe {
			continue
		}
		total += 1
		if looping {
			inLoop += 1
		}
	}
	return total, inLoop
}

// The frame shapes below are copied from runtime.Stack output. The first is the
// blind spot this matcher had: a goroutine created by the start but not yet
// scheduled shows the generated wrapper, not the closure, so matching only
// startSmartDialerProbes.func1 silently counted zero and the off-case test could
// pass with probe goroutines already started.
func TestSmartDialerProbeFramesSeesAnUnscheduledGoroutine(t *testing.T) {
	unscheduled := `goroutine 6 [runnable]:
github.com/urnetwork/connect/provider.startSmartDialerProbes.gowrap1()
	/provider/smart_dialer_probe.go:60
runtime.goexit({})
	/usr/lib/go/src/runtime/asm_amd64.s:1264 +0x1
created by github.com/urnetwork/connect/provider.startSmartDialerProbes in goroutine 82
	/provider/smart_dialer_probe.go:60 +0x165`

	scheduled := `goroutine 7 [select, 4 minutes]:
github.com/urnetwork/connect/provider.runSmartDialerProbes({0x1234, 0x5678}, 0x3b9aca00, 0x12a05f200, 0xabc, 0xdef)
	/provider/smart_dialer_probe.go:88 +0x1a5
github.com/urnetwork/connect/provider.startSmartDialerProbes.func1()
	/provider/smart_dialer_probe.go:61 +0x2c
created by github.com/urnetwork/connect/provider.startSmartDialerProbes in goroutine 82
	/provider/smart_dialer_probe.go:60 +0x165`

	other := `goroutine 8 [chan receive]:
github.com/urnetwork/connect.(*ClientStrategy).serve(0xc000123456)
	/net_http.go:900 +0x45
created by github.com/urnetwork/connect.NewClientStrategy in goroutine 1
	/net_http.go:120 +0x1a5`

	if probe, _ := probeFrames(unscheduled); !probe {
		t.Error("an unscheduled probe goroutine (gowrap1, no func1) is not counted")
	}
	if probe, inLoop := probeFrames(scheduled); !probe || !inLoop {
		t.Errorf("a probe goroutine parked in the loop = probe %v, inLoop %v; want true, true", probe, inLoop)
	}
	if probe, inLoop := probeFrames(other); probe || inLoop {
		t.Error("an unrelated goroutine is counted as a probe goroutine")
	}
}

// The defect this guards: with the preference off the probe loop is a no-op, so
// spawning it per proxy bought nothing and cost one goroutine plus its stack for
// every proxy on the node. Nothing may be started at all — proved from the
// goroutine stacks, not inferred from the preference being read.
func TestSmartDialerProbesStartNoGoroutineWhenOff(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())

	// Let the strategy's own construction settle so the count below moves only
	// if a probe start adds something.
	time.Sleep(25 * time.Millisecond)
	if total, _ := probeGoroutines(); total != 0 {
		t.Fatalf("%d probe goroutines before any start", total)
	}
	before := runtime.NumGoroutine()

	// One call per identity, as provide() does for every proxy.
	const identities = 64
	for i := 0; i < identities; i += 1 {
		startSmartDialerProbes(ctx, strategy, "http://127.0.0.1:0/probe", fmt.Sprintf("proxy[%d]", i))
	}

	// A started goroutine reaches the loop on its first turn; poll so a late
	// schedule cannot hide it, and fail the moment one appears.
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if total, _ := probeGoroutines(); total != 0 {
			t.Fatalf("smart dialer off, but %d probe goroutines were started for %d identities", total, identities)
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); before < after {
		t.Fatalf("goroutine count grew from %d to %d across %d starts with the smart dialer off", before, after, identities)
	}
}

// With the preference on the behaviour is exactly what it was: one loop per
// identity, parked in the loop until its delay elapses, and gone once the
// identity's context is canceled.
func TestSmartDialerProbesStartLoopWhenOnAndStopCleanly(t *testing.T) {
	previous := connect.SetSmartDialer(true)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())

	if total, _ := probeGoroutines(); total != 0 {
		t.Fatalf("%d probe goroutines before any start", total)
	}

	const identities = 3
	for i := 0; i < identities; i += 1 {
		startSmartDialerProbes(ctx, strategy, "http://127.0.0.1:0/probe", fmt.Sprintf("proxy[%d]", i))
	}

	// One goroutine per identity, and it must be inside the loop waiting out
	// the initial delay -- not merely created and forgotten.
	deadline := time.Now().Add(2 * time.Second)
	for {
		total, inLoop := probeGoroutines()
		if total == identities && inLoop == identities {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d probe goroutines (%d inside the loop), want %d of each", total, inLoop, identities)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if total, _ := probeGoroutines(); total == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			total, inLoop := probeGoroutines()
			t.Fatalf("%d probe goroutines (%d inside the loop) still running after the context was canceled", total, inLoop)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The loop waits out the initial delay (so a starting proxy is not probed
// while it is still authenticating), then probes on every tick until the
// context ends.
func TestSmartDialerProbeLoopProbesAfterDelayThenRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartDialerProbes(ctx, 20*time.Millisecond, 10*time.Millisecond, func(context.Context) int {
			calls.Add(1)
			return 0
		}, func(string, ...any) {})
	}()

	time.Sleep(5 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("probed %d times inside the initial delay, want 0", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("probed %d times, want at least 3 (initial then repeating)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe loop did not stop when the context was canceled")
	}
}

// A round that attempted nothing (smart dialer off, or every transport already
// measured) is silent; a round that probed says so once.
func TestSmartDialerProbeLoopLogsOnlyRoundsThatProbed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var rounds atomic.Int32
	var logged atomic.Int32
	go runSmartDialerProbes(ctx, 0, 5*time.Millisecond, func(context.Context) int {
		if rounds.Add(1) == 2 {
			return 3
		}
		return 0
	}, func(string, ...any) { logged.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for rounds.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if got := logged.Load(); got != 1 {
		t.Fatalf("logged %d times over %d rounds, want exactly 1 (the round that probed 3)", got, rounds.Load())
	}
}
