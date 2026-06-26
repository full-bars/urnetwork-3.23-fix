package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// These tests cover the launch-time enforcement of the URL-sourced proxy
// give-up backoff. Before this, proxyURLGiveUpRetryDelay computed an
// escalating delay and scheduled a one-shot reload via time.AfterFunc, but
// nothing stopped any *other* reload (another proxy's give-up, a URL refresh)
// from relaunching a dead proxy immediately. Live fleet data showed proxies
// reaching give-up 9 within 44 minutes of uptime, i.e. relaunched every few
// minutes instead of waiting their 15m->24h backoff. The gate below records a
// per-address "next eligible launch time" so the reload path can skip a proxy
// whose backoff has not yet elapsed.

func TestProxyFailureHistory_Eligible_DefaultsTrueForUnmarkedAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if !h.Eligible("1.2.3.4:1080", time.Now()) {
		t.Fatal("an address with no recorded backoff should be eligible to launch")
	}
}

func TestProxyFailureHistory_SetBackoffUntil_GatesUntilTimeElapses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(time.Hour))

	if h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("address should NOT be eligible before its backoff window elapses")
	}
	if h.Eligible("1.2.3.4:1080", now.Add(30*time.Minute)) {
		t.Fatal("address should NOT be eligible partway through its backoff window")
	}
	if !h.Eligible("1.2.3.4:1080", now.Add(time.Hour)) {
		t.Fatal("address should be eligible exactly at its next-eligible time")
	}
	if !h.Eligible("1.2.3.4:1080", now.Add(2*time.Hour)) {
		t.Fatal("address should be eligible after its backoff window elapses")
	}
	if !h.Eligible("5.6.7.8:1080", now) {
		t.Fatal("a different address must be unaffected by another's backoff")
	}
}

func TestProxyFailureHistory_Reset_ClearsBackoff(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour))
	h.Reset("1.2.3.4:1080")

	if !h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("a proxy that succeeds (Reset) must immediately become eligible again")
	}
}

func TestProxyFailureHistory_Prune_RemovesBackoffForDroppedAddresses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour))
	h.SetBackoffUntil("5.6.7.8:1080", now.Add(24*time.Hour))

	h.Prune(map[string]bool{"1.2.3.4:1080": true})

	if h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("kept address must retain its backoff window")
	}
	if !h.Eligible("5.6.7.8:1080", now) {
		t.Fatal("dropped address's backoff must be pruned (eligible again)")
	}
}

// TestReload_SkipsURLProxyStillInBackoff is the launch-time enforcement test:
// a URL-sourced proxy whose give-up backoff window has not elapsed must NOT be
// relaunched by reload(), even though it is "desired but not running". Before
// this gate, any reload (e.g. triggered by another proxy's give-up) relaunched
// every dead proxy immediately, so the escalating backoff never took effect.
func TestReload_SkipsURLProxyStillInBackoff(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"7.7.7.7:1080": {}, // mid-backoff, must be skipped
		"8.8.8.8:1080": {}, // eligible, must still start
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	globalProxyFailureHistory.Reset("7.7.7.7:1080")
	globalProxyFailureHistory.Reset("8.8.8.8:1080")
	globalProxyFailureHistory.SetBackoffUntil("7.7.7.7:1080", time.Now().Add(time.Hour))
	defer globalProxyFailureHistory.Reset("7.7.7.7:1080")

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
	_, backoffStarted := reloader.cancelMap["7.7.7.7:1080"]
	_, eligibleStarted := reloader.cancelMap["8.8.8.8:1080"]
	cancelMapMu.Unlock()

	if backoffStarted {
		t.Fatal("a URL proxy still within its give-up backoff window must NOT be relaunched by reload()")
	}
	if !eligibleStarted {
		t.Fatal("an eligible URL proxy must still be launched by reload()")
	}
}
