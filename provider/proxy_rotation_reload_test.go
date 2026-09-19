package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// bootLaunchedReloader builds a reloader whose one proxy was launched by the
// startup loop (present in cancelMap) against the given proxy file, and
// returns a counter of how many times that proxy's goroutine was cancelled.
func bootLaunchedReloader(t *testing.T, file string, boot *connect.ProxySettings) (*ProxyReloader, *atomic.Int32) {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	var cancelled atomic.Int32
	cancel := context.CancelFunc(func() { cancelled.Add(1) })
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{boot.Address: cancel},
		cancelMapMu: &sync.Mutex{},
		runningAuth: make(map[string]*connect.ProxySettings),
		state:       &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:  file,
		parentCtx:   parent,
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}
	// Replacement goroutines block in spawnProxy until their context is
	// cancelled; stop them and wait, so their deferred unregistration runs and
	// nothing outlives the test.
	t.Cleanup(func() {
		cancelParent()
		r.wg.Wait()
	})
	return r, &cancelled
}

func writeProxyFile(t *testing.T, line string) string {
	t.Helper()
	f := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(f, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A seeded, boot-launched proxy whose credentials are unchanged must survive
// the first reload untouched.
func TestReload_SeededBootProxy_NotRestarted(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "192.0.2.1:1080:alice:secret"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	r.reload()

	if n := cancelled.Load(); n != 0 {
		t.Fatalf("unchanged boot-launched proxy was cancelled %d time(s) by the first reload", n)
	}
}

// Pins why seeding must precede the first reload: an unseeded boot-launched
// proxy is treated as unknown and rotated.
func TestReload_UnseededBootProxy_IsRotated(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "192.0.2.1:1080:alice:secret"), boot)

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("expected the unseeded proxy to be cancelled once, got %d", n)
	}
}

// A rotated proxy with active clients must be cancelled immediately, not
// drained: draining keeps the old credentials serving until the last client
// leaves, and the launch pass skips addresses that are still draining.
func TestReload_RotatedBusyProxy_IsNotDrained(t *testing.T) {
	const addr = "192.0.2.7:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, addr+":alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	connect.RegisterProxy(987001, addr)
	bw := connect.RegisterProxyBandwidth(987001)
	t.Cleanup(func() { connect.UnregisterProxy(987001) })
	bw.Clients.Store(3) // active sessions on the old credentials

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("busy rotated proxy: expected old goroutine cancelled once, got %d", n)
	}
	if r.isDraining(addr) {
		t.Fatal("rotated proxy must not enter the draining state")
	}
	got, ok := r.runningAuthFor(addr)
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}

// Rotating credentials relaunches the proxy in the same pass; it must keep its
// state entry so the relaunch reuses the stable ID and persisted health.
func TestReload_RotatedProxy_KeepsStateEntry(t *testing.T) {
	const addr = "192.0.2.20:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "test-user", Password: "test-pass"}}
	r, _ := bootLaunchedReloader(t, writeProxyFile(t, addr+":test-user:rotated-pass"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: {ID: 7, Health: "up"}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	r.state = state

	r.reload()

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	kept, ok := after.Proxies[addr]
	if !ok {
		t.Fatal("rotated proxy lost its state entry")
	}
	if kept.ID != 7 || kept.Health != "up" {
		t.Fatalf("rotated proxy must keep ID 7 and health up, got ID=%d health=%q", kept.ID, kept.Health)
	}
}

// A goroutine from a superseded launch must not unregister the health entry
// the replacement registered under the same stable ID.
func TestUnregisterProxyIfCurrent_StaleLaunchKeepsReplacement(t *testing.T) {
	const addr, idx = "192.0.2.30:1080", 987030
	connect.RegisterProxy(idx, addr)
	connect.RegisterProxyBandwidth(idx)
	t.Cleanup(func() { connect.UnregisterProxy(idx) })

	oldGen := beginProxyLaunch(addr)
	newGen := beginProxyLaunch(addr) // rotation relaunch
	unregisterProxyIfCurrent(addr, oldGen, idx)
	if connect.ProxyBandwidthByAddress(addr) == nil {
		t.Fatal("stale launch unregistered the replacement's health entry")
	}
	unregisterProxyIfCurrent(addr, newGen, idx)
	if connect.ProxyBandwidthByAddress(addr) != nil {
		t.Fatal("the owning launch must unregister on exit")
	}
}

// A cancelled goroutine must not delete the replacement's cancel-map entry.
func TestDeleteProxyCancelIfCurrent(t *testing.T) {
	const addr = "192.0.2.31:1080"
	mu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{addr: func() {}}

	oldCtx := withProxyLaunchGen(context.Background(), beginProxyLaunch(addr))
	newCtx := withProxyLaunchGen(context.Background(), beginProxyLaunch(addr))
	t.Cleanup(func() { proxyLaunches.mu.Lock(); delete(proxyLaunches.current, addr); proxyLaunches.mu.Unlock() })

	deleteProxyCancelIfCurrent(mu, cancelMap, oldCtx, addr)
	if _, ok := cancelMap[addr]; !ok {
		t.Fatal("stale launch deleted the replacement's cancel-map entry")
	}
	deleteProxyCancelIfCurrent(mu, cancelMap, newCtx, addr)
	if _, ok := cancelMap[addr]; ok {
		t.Fatal("current launch must be able to delete its own entry")
	}

	cancelMap[addr] = func() {}
	deleteProxyCancelIfCurrent(mu, cancelMap, context.Background(), addr)
	if _, ok := cancelMap[addr]; ok {
		t.Fatal("a context without a generation keeps the unconditional delete")
	}
}

// End-to-end rotation with the reloader's real goroutines: launch a proxy
// through reload(), rotate its credentials, and let the superseded goroutine
// unwind. The replacement reuses the stable ID, so the old goroutine's exit
// must not remove the replacement's health registration or cancel-map entry.
// This is the interleaving that unit tests with fake boot entries cannot see.
func TestReload_RotationWithRealGoroutines_KeepsRegistration(t *testing.T) {
	const addr = "192.0.2.40:1080"
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	file := writeProxyFile(t, addr+":test-user:test-pass")
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:       map[string]context.CancelFunc{},
		cancelMapMu:     &sync.Mutex{},
		runningAuth:     map[string]*connect.ProxySettings{},
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		drainingProxies: map[string]context.CancelFunc{},
		spawnProxy: func(ctx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-ctx.Done()
		},
	}
	t.Cleanup(func() {
		cancelParent()
		r.wg.Wait()
		// leave no health registration behind for later tests
		if _, registered := connect.ProxyHealthByAddress()[addr]; registered {
			connect.UnregisterProxy(r.state.Proxies[addr].ID)
		}
	})

	r.reload() // launch
	if _, ok := r.state.Proxies[addr]; !ok {
		t.Fatal("first reload did not create a state entry")
	}
	if _, registered := connect.ProxyHealthByAddress()[addr]; !registered {
		t.Fatal("first reload did not register the proxy")
	}

	if err := os.WriteFile(file, []byte(addr+":test-user:rotated-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.reload() // rotate: cancels the first goroutine, relaunches under the same ID

	// The superseded goroutine exits promptly once cancelled. Keep watching
	// for a window well beyond that: the registration must survive it.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, registered := connect.ProxyHealthByAddress()[addr]; !registered {
			t.Fatal("the superseded goroutine's exit removed the replacement's health registration")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.cancelMapMu.Lock()
	_, running := r.cancelMap[addr]
	r.cancelMapMu.Unlock()
	if !running {
		t.Fatal("replacement is missing from the cancel map")
	}
	got, ok := r.runningAuthFor(addr)
	if !ok || got.Auth == nil || got.Auth.Password != "rotated-pass" {
		t.Fatalf("replacement must run with the rotated credentials, got %+v ok=%v", got, ok)
	}
}
