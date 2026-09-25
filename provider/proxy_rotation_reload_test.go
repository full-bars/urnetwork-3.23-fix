package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
		cancelMap:   map[string]context.CancelFunc{boot.Key(): cancel},
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

// TestReloader builds a minimal reloader with no boot proxy, for cases where
// reload() should find an empty desired set. Cancellation is a no-op counter.
func emptyReloader(t *testing.T, sourcePath string) *ProxyReloader {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:       map[string]context.CancelFunc{},
		cancelMapMu:     &sync.Mutex{},
		runningAuth:     make(map[string]*connect.ProxySettings),
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      sourcePath,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) { <-parent.Done() },
		drainingProxies: map[string]context.CancelFunc{},
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })
	return r
}

// A direct-only node (no proxy source configured) reads a VALID, settled
// zero-proxy state, not "empty" degraded. This is the fix for the direct-only
// node staying degraded forever: a deliberate direct-only config is not a
// source that was queried and came back empty.
func TestReload_DirectOnlyNode_SettlesZeroValid(t *testing.T) {
	resetProxyCounters(t)

	// sourcePath "" = no --proxy_file source; no proxy_url.json in the temp
	// home = no URL sources. So anySourceConfigured is false and the reload
	// records the valid-zero (direct-only) state.
	r := emptyReloader(t, "")
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionZeroValid {
		t.Fatalf("direct-only reload: resolution=%d, want proxyResolutionZeroValid(%d)", got, proxyResolutionZeroValid)
	}
	if phase := proxyStartupPhase(); phase != "" {
		t.Fatalf("direct-only node must settle (empty startup phase), got %q", phase)
	}
	if line := systemdStatusLine(); !strings.HasPrefix(line, "active:") {
		t.Fatalf("direct-only node must read active, got %q", line)
	}
}

// With the direct transport turned off and no proxy source configured,
// nothing is served, so the node must not settle as a healthy direct-only
// node: it reads degraded (empty), not active.
func TestReload_DirectOffNoSource_StillReadsEmpty(t *testing.T) {
	resetProxyCounters(t)

	t.Setenv("DISABLE_DIRECT_IP", "1") // direct transport off
	r := emptyReloader(t, "")
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionEmpty {
		t.Fatalf("direct-off no-source reload: resolution=%d, want proxyResolutionEmpty(%d)", got, proxyResolutionEmpty)
	}
	if phase := proxyStartupPhase(); phase != startupSourceEmpty {
		t.Fatalf("direct-off no-source node must read empty/degraded, got %q", phase)
	}
	if line := systemdStatusLine(); strings.HasPrefix(line, "active:") {
		t.Fatalf("direct-off no-source node must NOT read active while serving nothing, got %q", line)
	}
}

// A proxy source that WAS configured but yielded zero proxies still reads
// degraded (empty), even though the node may run direct alongside it — it is
// not a deliberate direct-only config.
func TestReload_EmptySource_StillReadsEmpty(t *testing.T) {
	resetProxyCounters(t)

	// A configured source with only blank lines = a source that returned zero
	// proxies (configured, so not direct-only).
	r := emptyReloader(t, writeProxyFile(t, "# empty"))
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionEmpty {
		t.Fatalf("empty-source reload: resolution=%d, want proxyResolutionEmpty(%d)", got, proxyResolutionEmpty)
	}
	if phase := proxyStartupPhase(); phase != startupSourceEmpty {
		t.Fatalf("empty-source node must read empty/degraded, got %q", phase)
	}
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
	key := boot.Key()
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, addr+":alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	connect.RegisterProxy(987001, addr, key)
	bw := connect.RegisterProxyBandwidth(987001)
	t.Cleanup(func() { connect.UnregisterProxy(987001) })
	bw.Clients.Store(3) // active sessions on the old credentials

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("busy rotated proxy: expected old goroutine cancelled once, got %d", n)
	}
	if r.isDraining(key) {
		t.Fatal("rotated proxy must not enter the draining state")
	}
	got, ok := r.runningAuthFor(key)
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}

// Rotating credentials relaunches the proxy in the same pass; it must keep its
// state entry so the relaunch reuses the stable ID and persisted health.
func TestReload_RotatedProxy_KeepsStateEntry(t *testing.T) {
	const addr = "192.0.2.20:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "test-user", Password: "test-pass"}}
	key := boot.Key()
	r, _ := bootLaunchedReloader(t, writeProxyFile(t, addr+":test-user:rotated-pass"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	state := &ProxyState{Proxies: map[string]ProxyEntry{key: {ID: 7, Health: "up"}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	r.state = state

	r.reload()

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	kept, ok := after.Proxies[key]
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
	connect.RegisterProxy(idx, addr, addr)
	connect.RegisterProxyBandwidth(idx)
	t.Cleanup(func() { connect.UnregisterProxy(idx) })

	oldGen := beginProxyLaunch(addr)
	newGen := beginProxyLaunch(addr) // rotation relaunch
	unregisterProxyIfCurrent(addr, oldGen, idx)
	if connect.ProxyBandwidthByKey(addr) == nil {
		t.Fatal("stale launch unregistered the replacement's health entry")
	}
	unregisterProxyIfCurrent(addr, newGen, idx)
	if connect.ProxyBandwidthByKey(addr) != nil {
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
	key := (&connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: "test-user"}}).Key()
	withTempHome(t)
	// Production seeds the ID counter at startup so ID 0 stays reserved for
	// [direct]. Without it, run in isolation the first proxy is handed ID 0
	// and races the direct goroutine's RegisterProxy(0, "direct"), which
	// overwrites this proxy's health entry.
	initProxyIDCounter(0)
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
		if _, registered := connect.ProxyHealthByKey()[key]; registered {
			connect.UnregisterProxy(r.state.Proxies[key].ID)
		}
	})

	r.reload() // launch
	if _, ok := r.state.Proxies[key]; !ok {
		t.Fatal("first reload did not create a state entry")
	}
	// The launch loop registers the health entry synchronously inside
	// reload(), but a scheduled goroutine from a preceding test can still be
	// unwinding on the suite's shared health map when this reload returns.
	// Watch for the entry instead of asserting on the exact instruction:
	// the launch must register promptly, and a timeout here is a genuine
	// launch regression (same watch pattern as the rotation assert below).
	regDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, registered := connect.ProxyHealthByKey()[key]; registered {
			break
		}
		if time.Now().After(regDeadline) {
			_, ok := r.state.Proxies[key]
			t.Fatalf("first reload did not register the proxy within 2s (eligible=%v giveups=%d state_has=%v)", globalProxyFailureHistory.Eligible(key, time.Now()), globalProxyFailureHistory.GiveUpCount(key), ok)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := os.WriteFile(file, []byte(addr+":test-user:rotated-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.reload() // rotate: cancels the first goroutine, relaunches under the same ID

	// The superseded goroutine exits promptly once cancelled. Keep watching
	// for a window well beyond that: the registration must survive it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, registered := connect.ProxyHealthByKey()[key]; !registered {
			t.Fatal("the superseded goroutine's exit removed the replacement's health registration")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.cancelMapMu.Lock()
	_, running := r.cancelMap[key]
	r.cancelMapMu.Unlock()
	if !running {
		t.Fatal("replacement is missing from the cancel map")
	}
	got, ok := r.runningAuthFor(key)
	if !ok || got.Auth == nil || got.Auth.Password != "rotated-pass" {
		t.Fatalf("replacement must run with the rotated credentials, got %+v ok=%v", got, ok)
	}
}

// TestReload_SameAddressDifferentUser_BothRunStably is the actual
// production incident this fix targets: two accounts sharing one gateway
// address (Decodo's model) must both launch, both stay in cancelMap, and
// NEITHER may be perpetually rotated across repeated reload cycles. Before
// this fix, desiredSet was keyed by bare Address, so Go's randomized map
// iteration order picked a different "winner" every reload, looking like a
// credential change and rotating forever.
func TestReload_SameAddressDifferentUser_BothRunStably(t *testing.T) {
	const addr = "dc.decodo.com:10058"
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	userA := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "pw1"}}
	userB := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "user-sppmr4vcnj-country-us-city-metro", Password: "pw2"}}
	keyA, keyB := userA.Key(), userB.Key()
	if keyA == keyB {
		t.Fatal("test setup bug: keyA and keyB must differ")
	}

	file := writeProxyFile(t, addr+":sppmr4vcnj:pw1\n"+addr+":user-sppmr4vcnj-country-us-city-metro:pw2")
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
	})

	r.reload() // launch both

	r.cancelMapMu.Lock()
	_, hasA := r.cancelMap[keyA]
	_, hasB := r.cancelMap[keyB]
	r.cancelMapMu.Unlock()
	if !hasA || !hasB {
		t.Fatalf("both identities must be in cancelMap after the first reload: hasA=%v hasB=%v", hasA, hasB)
	}

	// 10 consecutive reloads with an unchanged config must rotate NEITHER
	// identity: this is the exact storm this fix stops. bootLaunchedReloader-
	// style tests already prove a single-identity address stays stable; this
	// proves two identities at the SAME address are both independently
	// stable, not just one of them by chance.
	for i := 0; i < 10; i++ {
		r.reload()
		r.cancelMapMu.Lock()
		_, hasA = r.cancelMap[keyA]
		_, hasB = r.cancelMap[keyB]
		r.cancelMapMu.Unlock()
		if !hasA || !hasB {
			t.Fatalf("reload #%d: an identity dropped out of cancelMap (hasA=%v hasB=%v) — this is the perpetual-rotation storm", i+1, hasA, hasB)
		}
	}

	gotA, okA := r.runningAuthFor(keyA)
	gotB, okB := r.runningAuthFor(keyB)
	if !okA || gotA.Auth.Password != "pw1" {
		t.Errorf("identity A lost or changed its credentials: %+v ok=%v", gotA, okA)
	}
	if !okB || gotB.Auth.Password != "pw2" {
		t.Errorf("identity B lost or changed its credentials: %+v ok=%v", gotB, okB)
	}

	// A genuine password-only change for ONE identity must still rotate
	// exactly that one identity, leaving the other untouched — the fix
	// must not have traded "storm" for "rotation never works at all".
	if err := os.WriteFile(file, []byte(addr+":sppmr4vcnj:NEWPW\n"+addr+":user-sppmr4vcnj-country-us-city-metro:pw2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.reload()
	gotA, okA = r.runningAuthFor(keyA)
	if !okA || gotA.Auth.Password != "NEWPW" {
		t.Fatalf("identity A's genuine credential change was not picked up: %+v ok=%v", gotA, okA)
	}
	gotB, okB = r.runningAuthFor(keyB)
	if !okB || gotB.Auth.Password != "pw2" {
		t.Fatalf("identity B was disturbed by identity A's unrelated rotation: %+v ok=%v", gotB, okB)
	}
}

// proxy_url.json that exists but cannot be read leaves the URL sources unknown.
// A URL-only node must not read that as a settled, valid direct-only config: it
// is a source that could not be resolved. (A MISSING file is a different thing:
// no URL sources, direct-only, settled; the test above pins that.)
func TestReload_UnreadableURLState_IsNotDirectOnly(t *testing.T) {
	resetProxyCounters(t)
	r := emptyReloader(t, "")
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionFailed {
		t.Fatalf("unreadable URL state: resolution=%d, want proxyResolutionFailed(%d), not the settled direct-only state", got, proxyResolutionFailed)
	}
	if phase := proxyStartupPhase(); phase != startupSourceUnreachable {
		t.Fatalf("startup phase = %q, want %q", phase, startupSourceUnreachable)
	}
	if line := systemdStatusLine(); strings.HasPrefix(line, "active:") {
		t.Fatalf("a node whose URL sources cannot be read must not read active, got %q", line)
	}
}
