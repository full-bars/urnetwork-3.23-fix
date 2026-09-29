package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// urlCancelFixture is a reloader with ONE running URL-sourced proxy and a
// file-backed source that supplies a different proxy. The URL proxy is running
// but is NOT in the file source, so it only appears in desiredSet when the URL
// cache is merged in. That is the shape the cancel bug needs: with the cache
// read failing, the URL proxy is absent from desiredSet and would be cancelled.
func urlCancelFixture(t *testing.T) (*ProxyReloader, *atomic.Int32) {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	// The file source supplies exactly one proxy.
	fileAddr := &connect.ProxySettings{Address: "10.0.0.1:1080", Auth: &proxy.Auth{User: "u", Password: "p"}}

	// The URL proxy is running but comes from nowhere the file knows about.
	urlSettings := &connect.ProxySettings{Address: "8.8.8.8:1080", Auth: &proxy.Auth{User: "v", Password: "q"}}
	urlKey := urlSettings.Key()

	file := filepath.Join(t.TempDir(), "proxy.txt")
	if err := os.WriteFile(file, []byte(fileAddr.Address+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Int32
	cancel := context.CancelFunc(func() { cancelled.Add(1) })
	parent, cancelParent := context.WithCancel(context.Background())

	r := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{urlKey: cancel},
		cancelMapMu: &sync.Mutex{},
		runningAuth: map[string]*connect.ProxySettings{urlKey: urlSettings},
		state: &ProxyState{Proxies: map[string]ProxyEntry{
			urlKey:         {ID: 9, Health: "up", Graded: true, Score: 0.9},
			fileAddr.Key(): {ID: 1, Health: "up"},
		}},
		sourcePath:      file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) {},
		drainingProxies: map[string]context.CancelFunc{},
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	return r, &cancelled
}

// With proxy_url.json unreadable, the URL cache is never merged into the desired
// set, so the running URL proxy looks absent and the reload used to cancel it.
// One transient read error on the state file tore down the live URL pool.
func TestReloadDoesNotCancelTheURLPoolWhenTheCacheCannotBeRead(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, cancelled := urlCancelFixture(t)

	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLStateForMerge(); ok {
		t.Fatal("the fixture must produce an unreadable state, or this test proves nothing")
	}

	before := cancelled.Load()
	out := captureTlog(t, func() { r.reload() })
	after := cancelled.Load()

	if after != before {
		t.Fatalf("a failed proxy_url.json read must not cancel the running URL pool: cancelled went %d -> %d\n%s", before, after, out)
	}
}

// A proxy_url.json that cannot be read must NOT be treated as an empty one.
// The merge cycle used to substitute a fresh empty state and carry on, then
// write it back, which destroyed every cached entry not re-listed that cycle,
// every persisted grade, and the entire Blacklist map. The blacklist is
// add-only by design (a fetch never repopulates it), so that wipe silently
// undid every permanent eviction and lost every grade earned.
func TestUnreadableURLStateMakesTheMergeSkipRatherThanWipe(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory at the state path is a genuine read error (EISDIR), not
	// "the file is absent", which correctly yields an empty state.
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readProxyURLState(); err == nil {
		t.Fatal("the fixture must produce a read error, or this test proves nothing")
	}

	if state, ok := readURLStateForMerge(); ok {
		t.Fatalf("an unreadable state must not be reported as usable, got %+v", state)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("nothing may be written over an unreadable state path (stat err=%v)", err)
	}
}

// The control case, so the test above is not passing for the wrong reason: a
// MISSING file is normal (first start) and must still yield a usable empty
// state, or the fetch cycle would never run on a fresh box.
func TestMissingURLStateStillYieldsAUsableEmptyState(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	state, ok := readURLStateForMerge()
	if !ok {
		t.Fatal("a missing file is normal and must not block the merge cycle")
	}
	if state == nil || state.Cache == nil {
		t.Fatal("a usable empty state with an initialised cache must be returned")
	}
}

// A healthy file must round-trip intact: cache entries, the permanent
// blacklist, and the source list.
func TestSeededURLStateSurvivesAFailedMergeCycle(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := &ProxyURLState{
		Cache:     map[string]ProxyURLEntry{"1.2.3.4:1080": {Score: 0.93, Graded: true, ProbeOK: true}},
		Sources:   []string{"https://example.invalid/list.txt"},
		Blacklist: map[string]time.Time{"9.9.9.9:1080": time.Unix(1700000000, 0).UTC()},
	}
	if err := writeProxyURLState(seed); err != nil {
		t.Fatal(err)
	}
	got, ok := readURLStateForMerge()
	if !ok {
		t.Fatal("a healthy file must be usable")
	}
	if len(got.Cache) != 1 {
		t.Errorf("cache entries must survive, got %d", len(got.Cache))
	}
	if _, ok := got.Blacklist["9.9.9.9:1080"]; !ok {
		t.Error("the blacklist must survive: a fetch never repopulates it")
	}
	if len(got.Sources) != 1 {
		t.Errorf("sources must survive, got %d", len(got.Sources))
	}
}
