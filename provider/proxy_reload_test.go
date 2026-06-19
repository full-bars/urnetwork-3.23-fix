package main

import (
	"context"
	"sync"
	"testing"

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
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool) {
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
