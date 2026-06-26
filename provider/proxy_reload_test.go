package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestReload_URLOnlySource_NoEarlyExit is a regression test for a bug found
// during live deployment testing: when there is no --proxy_file and no
// internal proxies (Workflow A/B empty), reload() used to bail out before
// ever merging in the URL-sourced cache, so a URL-only deployment could
// never start any proxies via hot-reload.
func TestReload_URLOnlySource_NoEarlyExit(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"5.5.5.5:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()

	cancelMapMu.Lock()
	_, started := reloader.cancelMap["5.5.5.5:1080"]
	cancelMapMu.Unlock()
	if !started {
		t.Fatal("expected URL-sourced proxy to be started by reload() even with no file/internal proxies configured")
	}
}

// TestReload_AddedProxies_NoPerProxyEnumeration verifies reload() does NOT
// print one line per added proxy. The per-proxy enumeration was found in fleet
// log analysis to be a dominant ramlog flooder: a reload of hundreds/thousands
// of (mostly dead, churning) proxies dumped that many lines per reload via raw
// fmt.Printf, flushing high-value lines ([profit]/[earn]/[contract]) out of the
// small in-RAM buffer within seconds. The terse "[proxy] reloaded: +N -M"
// summary carries the operator-relevant signal; the per-proxy roster on every
// reload is noise. (The cold-start "Using N proxy servers:" roster is kept; it
// prints once, before any earning, and documents what the provider started
// with.)
func TestReload_AddedProxies_NoPerProxyEnumeration(t *testing.T) {
	withTempHome(t)

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	tmpFile := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(tmpFile, []byte("5.5.5.5:1080:alice:secret\n6.6.6.6:1080:bob:hunter2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  tmpFile,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	reloader.reload()
	os.Stdout = origStdout
	w.Close()
	out, _ := io.ReadAll(r)

	got := string(out)
	if !strings.Contains(got, "reloaded: +2 added") {
		t.Fatalf("expected reload to print the summary count, got: %q", got)
	}
	if strings.Contains(got, "5.5.5.5:1080") || strings.Contains(got, "6.6.6.6:1080") {
		t.Fatalf("reload must NOT enumerate each added proxy address (ramlog flood), got: %q", got)
	}
}

// TestReload_AddedProxies_UseJitteredBackoffPacer is a regression test for a
// second issue found in the same live deployment test: reload()'s "start
// added proxies" loop staggered startups by a fixed, unjittered 100ms * i —
// ten times faster than the jittered ~1s default used by the initial startup
// path (backoffPacer in main.go). A large batch merged in at once (e.g.
// hundreds of proxies from a URL source) would burst far faster than a fresh
// process start, overwhelming the auth API with simultaneous requests.
// reload() must use the same backoffPacer so a hot-reload ramp-up is exactly
// as gradual as a cold start.
func TestReload_AddedProxies_UseJitteredBackoffPacer(t *testing.T) {
	withTempHome(t)

	urlCache := map[string]ProxyURLEntry{}
	for i := range 6 {
		urlCache[fmt.Sprintf("10.0.0.%d:1080", i)] = ProxyURLEntry{}
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: urlCache}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	var spawnCount atomic.Int32
	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			spawnCount.Add(1)
			<-proxyCtx.Done()
		},
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	// URL proxies use 500ms stagger. Position 5 at 500ms*5 = 2500ms base
	// with ±250ms jitter = [2250, 2750]ms. Wait 3000ms, all 6 should be up.
	time.Sleep(3000 * time.Millisecond)
	if got := spawnCount.Load(); got != 6 {
		t.Fatalf("spawnProxy calls after 3000ms: got %d, want 6 (URL stagger is 500ms, position 5 starts at ~2500ms)", got)
	}
}

// TestReload_WarmupGate_DefersThenLaunchesURLProxies verifies that URL-
// sourced proxies are deferred during warmup and launched once warmup
// completes, confirming the warmup gate + reload trigger work together.
func TestReload_WarmupGate_DefersThenLaunchesURLProxies(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	// Start with warmup NOT done — URL proxies should be deferred
	proxyWarmupDone.Store(false)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	// First reload — warmup not done, URL proxy should be deferred
	reloader.reload()

	cancelMapMu.Lock()
	_, deferred := reloader.cancelMap["9.9.9.9:1080"]
	cancelMapMu.Unlock()
	if deferred {
		t.Fatal("URL proxy must NOT be launched during warmup")
	}

	// Mark warmup done and trigger a reload
	proxyWarmupDone.Store(true)
	if reloadPath, err := proxyReloadPath(); err == nil {
		_ = writeReloadTrigger(reloadPath)
	}
	reloader.reload()

	cancelMapMu.Lock()
	_, launched := reloader.cancelMap["9.9.9.9:1080"]
	cancelMapMu.Unlock()
	if !launched {
		t.Fatal("URL proxy must be launched after warmup completes")
	}
}
