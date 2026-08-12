package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRunProxyJWTWatcherRenewsOnTransportAuthFailures pins renewal trigger #4
// (documented on runProxyJWTWatcher): repeated PlatformTransport auth
// failures — separate from the OOB control path — must fast-path a renewal
// even when the cached token is far from its exp threshold and no OOB 401
// has ever been observed.
func TestRunProxyJWTWatcherRenewsOnTransportAuthFailures(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	// Token expiring in 30h: outside the 12h threshold, so only the
	// transport-auth-failure fast path can trigger renewal here.
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-authfail", clientJWTEntry{
		ByClientJWT: healthy,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	// A high, test-local proxy index to avoid colliding with any other
	// concurrently-run test's registration.
	const proxyIndex = 918273
	connect.RegisterProxy(proxyIndex, "test-proxy-authfail-addr")
	defer connect.UnregisterProxy(proxyIndex)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-authfail",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     proxyIndex,
		})
	}()

	// Let the watcher's synchronous startup check run (with 0 failures
	// recorded yet) before we record NEW failures, so the baseline it
	// snapshots is 0 and the failures below are all counted as "new".
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < revokedIdentityAuthFailureThreshold; i++ {
		connect.RecordProxyAuthFailure(proxyIndex, errors.New("401 Unauthorized"))
	}

	tick <- time.Now()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-authfail")
		if ok && entry.ByClientJWT != healthy {
			break // renewed
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew on the transport auth-failure fast path")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestRunProxyJWTWatcherDoesNotRenewOnStaleAuthFailures pins the baseline
// comparison (H-4): ProxyAuthFailureCount is a CUMULATIVE lifetime counter,
// so failures that occurred BEFORE the watcher started (and are therefore
// already part of the baseline) must NOT by themselves trigger a renewal —
// only NEW failures since the baseline was captured count.
func TestRunProxyJWTWatcherDoesNotRenewOnStaleAuthFailures(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-stale-authfail", clientJWTEntry{
		ByClientJWT: healthy,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	const proxyIndex = 918274
	connect.RegisterProxy(proxyIndex, "test-proxy-stale-authfail-addr")
	defer connect.UnregisterProxy(proxyIndex)

	// Failures recorded BEFORE the watcher starts become part of its
	// baseline snapshot.
	for i := 0; i < revokedIdentityAuthFailureThreshold; i++ {
		connect.RecordProxyAuthFailure(proxyIndex, errors.New("401 Unauthorized"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-stale-authfail",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     proxyIndex,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy-stale-authfail")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != healthy {
		t.Fatalf("watcher renewed on pre-existing (stale) auth failures — baseline comparison must ignore failures that predate the watcher's startup snapshot")
	}
	cancel()
	<-done
}
