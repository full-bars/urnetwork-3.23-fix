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

// TestRequeueURLProxyAfterGiveUp_URLSourceRemovedFromCancelMap is a
// regression test for a bug found during live deployment testing: a proxy
// that permanently gave up after exhausting its auth attempts stayed in
// cancelMap forever, so reload() could never see it as eligible to restart —
// contradicting the (inaccurate) "retry on next hourly pulse" log message.
// For url-sourced proxies, requeueURLProxyAfterGiveUp must remove the address
// from cancelMap immediately (making it eligible for reload() to re-add) and
// report that it queued an automatic retry.
func TestRequeueURLProxyAfterGiveUp_URLSourceRemovedFromCancelMap(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { clearGiveUpCooldown("5.5.5.5:1080") })

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"5.5.5.5:1080": {ID: 1, Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{
		"5.5.5.5:1080": func() {},
	}

	queued := requeueURLProxyAfterGiveUp(t.Context(), "5.5.5.5:1080", cancelMapMu, cancelMap)
	if !queued {
		t.Fatal("expected url-sourced proxy to be queued for automatic retry")
	}

	cancelMapMu.Lock()
	_, stillPresent := cancelMap["5.5.5.5:1080"]
	cancelMapMu.Unlock()
	if stillPresent {
		t.Fatal("expected address to be removed from cancelMap so reload() can re-add it")
	}
}

// TestRequeueURLProxyAfterGiveUp_NonURLSourceLeftAlone confirms file/internal
// sourced proxies are NOT automatically requeued — a permanent failure there
// likely indicates a real configuration problem the operator configured
// directly and should notice, not one that should retry silently forever.
func TestRequeueURLProxyAfterGiveUp_NonURLSourceLeftAlone(t *testing.T) {
	withTempHome(t)

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"6.6.6.6:1080": {ID: 1, Source: "file"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{
		"6.6.6.6:1080": func() {},
	}

	queued := requeueURLProxyAfterGiveUp(context.Background(), "6.6.6.6:1080", cancelMapMu, cancelMap)
	if queued {
		t.Fatal("expected file-sourced proxy to NOT be queued for automatic retry")
	}

	cancelMapMu.Lock()
	_, stillPresent := cancelMap["6.6.6.6:1080"]
	cancelMapMu.Unlock()
	if !stillPresent {
		t.Fatal("expected address to remain in cancelMap (no automatic retry for non-url sources)")
	}
}

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

// TestGiveUpCooldown_SetAndClear is a unit test for the cooldown bookkeeping
// itself, isolated from reload()'s diffing logic.
func TestGiveUpCooldown_SetAndClear(t *testing.T) {
	addr := "7.7.7.7:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })

	if isInGiveUpCooldown(addr) {
		t.Fatal("expected a fresh address to not be in cooldown")
	}

	setGiveUpCooldown(addr, time.Now().Add(time.Hour))
	if !isInGiveUpCooldown(addr) {
		t.Fatal("expected address to be in cooldown after setGiveUpCooldown")
	}

	clearGiveUpCooldown(addr)
	if isInGiveUpCooldown(addr) {
		t.Fatal("expected cooldown to be gone after clearGiveUpCooldown")
	}
}

// TestGiveUpCooldown_ExpiresOnItsOwn confirms a cooldown set in the past (or
// already elapsed) is reported as expired without an explicit clear — the
// 15-minute goroutine clears it proactively, but reload() must also treat a
// stale entry as expired on its own in case it's ever read first.
func TestGiveUpCooldown_ExpiresOnItsOwn(t *testing.T) {
	addr := "7.7.7.8:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })

	setGiveUpCooldown(addr, time.Now().Add(-time.Second))
	if isInGiveUpCooldown(addr) {
		t.Fatal("expected an already-elapsed cooldown to report as not in cooldown")
	}
}

// TestRequeueURLProxyAfterGiveUp_SetsCooldown is a regression test for a bug
// found during live deployment testing: a url-sourced proxy that gave up was
// removed from cancelMap immediately, but nothing actually enforced the
// promised 15-minute wait before reload() was allowed to relaunch it. Any
// unrelated reload trigger, like the periodic URL refresh or another
// proxy's own give-up timer, would resurrect it right away, observed
// retrying every 1-6 minutes instead of every 15.
func TestRequeueURLProxyAfterGiveUp_SetsCooldown(t *testing.T) {
	withTempHome(t)

	addr := "10.0.0.50:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		addr: {ID: 1, Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{addr: func() {}}

	if !requeueURLProxyAfterGiveUp(t.Context(), addr, cancelMapMu, cancelMap) {
		t.Fatal("expected url-sourced proxy to be queued for automatic retry")
	}

	if !isInGiveUpCooldown(addr) {
		t.Fatal("expected give-up to immediately start a 15-minute cooldown, not just removal from cancelMap")
	}
}

// TestScheduleGiveUpRequeue_RetriggersIfStillNotRunning is a regression test
// for a bug found during fleet log analysis: the original requeue goroutine
// fired its reload trigger exactly once. If that trigger landed during a
// transient reload() failure (e.g. its "reload skipped: could not read
// source" path), the address's cooldown was already cleared but nothing was
// left to ever bring it back — it sat orphaned until a manual 'proxy
// refresh' or process restart. scheduleGiveUpRequeue must check whether the
// address actually ended up back in cancelMap after the first trigger, and
// fire a second trigger if not.
func TestScheduleGiveUpRequeue_RetriggersIfStillNotRunning(t *testing.T) {
	withTempHome(t)

	addr := "10.0.0.51:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })

	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{} // addr never gets re-added, simulating a reload() that kept failing

	scheduleGiveUpRequeue(t.Context(), addr, cancelMapMu, cancelMap, 10*time.Millisecond, 20*time.Millisecond)

	seq, err := readReloadSeq(reloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("expected 2 reload triggers (initial + retry since addr never came back), got seq=%d", seq)
	}
}

// TestScheduleGiveUpRequeue_NoRetriggerIfRunning confirms the recheck is
// silent in the normal case: once the address is back in cancelMap after the
// first trigger, no second trigger should fire.
func TestScheduleGiveUpRequeue_NoRetriggerIfRunning(t *testing.T) {
	withTempHome(t)

	addr := "10.0.0.52:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })

	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{addr: func() {}} // already running by the time of the recheck

	scheduleGiveUpRequeue(t.Context(), addr, cancelMapMu, cancelMap, 10*time.Millisecond, 20*time.Millisecond)

	seq, err := readReloadSeq(reloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("expected exactly 1 reload trigger since addr was already running, got seq=%d", seq)
	}
}

// TestReload_SkipsAddressStillInGiveUpCooldown is the end-to-end regression
// test: even when an address is in desiredSet and absent from cancelMap
// (exactly the state right after a give-up), reload() must not relaunch it
// while its cooldown is still active — no matter what triggered this reload.
func TestReload_SkipsAddressStillInGiveUpCooldown(t *testing.T) {
	withTempHome(t)

	addr := "8.8.8.8:1080"
	t.Cleanup(func() { clearGiveUpCooldown(addr) })
	setGiveUpCooldown(addr, time.Now().Add(proxyURLGiveUpRetryAfter))

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		addr: {ID: 1, Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	var spawnCount atomic.Int32
	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{}, // not running — looks "added" by the old logic
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			spawnCount.Add(1)
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()
	time.Sleep(50 * time.Millisecond)

	if got := spawnCount.Load(); got != 0 {
		t.Fatalf("expected reload() to skip an address still in its give-up cooldown, got %d spawns", got)
	}

	cancelMapMu.Lock()
	_, started := reloader.cancelMap[addr]
	cancelMapMu.Unlock()
	if started {
		t.Fatal("expected the cooled-down address to not be added to cancelMap")
	}
}
