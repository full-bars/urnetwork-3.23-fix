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

// TestReload_AddedProxies_PrintsAddedList is a regression test for a usability
// gap found during live deployment testing: the initial startup path prints
// "Using N proxy servers:" followed by one line per proxy, but reload() only
// ever printed a terse "+N added, -N removed" summary with no per-proxy
// detail, making it hard to confirm a hot-reload actually picked up the
// proxies you expected (e.g. after editing a --proxy_file). reload() must
// print the same per-proxy listing style for added proxies.
func TestReload_AddedProxies_PrintsAddedList(t *testing.T) {
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
	if !strings.Contains(got, "[proxy] reload: adding 2 proxies:") {
		t.Fatalf("expected reload to announce the added count, got: %q", got)
	}
	if !strings.Contains(got, "5.5.5.5:1080") || !strings.Contains(got, "6.6.6.6:1080") {
		t.Fatalf("expected reload to list each added proxy address, got: %q", got)
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
	t.Setenv("URNETWORK_PROXY_STAGGER_MS", "50") // jitter +/-50ms around 50ms*position

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

	// Old hardcoded behavior: position 5 spawns at 100ms*5 = 500ms.
	// New jittered-backoffPacer behavior (staggerMs=50): position 5 spawns
	// within [250-50, 250+50] = [200, 300]ms. 350ms is comfortably past the
	// new upper bound and comfortably short of the old fixed 500ms, so this
	// distinguishes the two without being sensitive to scheduler jitter.
	time.Sleep(350 * time.Millisecond)
	if got := spawnCount.Load(); got != 6 {
		t.Fatalf("spawnProxy calls after 350ms: got %d, want 6 (all positions should have started under the jittered backoffPacer stagger)", got)
	}
}


