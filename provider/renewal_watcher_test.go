package main

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestRunProxyJWTWatcherRenewsOnExpiry(t *testing.T) {
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	// Seed a cached client JWT expiring in 1h (< 12h threshold).
	expiring := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-a", clientJWTEntry{
		ByClientJWT: expiring,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-a",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-a")
		if ok && entry.ByClientJWT != expiring {
			break // renewed
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew the expiring client JWT")
		case <-time.After(20 * time.Millisecond):
		}
	}

	entry, _ := store.Get("proxy-a")
	claims := decodeFakeJWTClaims(t, entry.ByClientJWT)
	if claims["client_id"] != clientID.String() {
		t.Fatalf("renewed JWT client_id = %v, want %q", claims["client_id"], clientID.String())
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRenewsOn401(t *testing.T) {
	// The fake server returns 401 for the FIRST OOB control call (so the OOB
	// counter increments), then 200 for the renewal auth-client call.
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	// Fresh token expiring in 30h: NOT within the 12h threshold, so only the
	// 401 fast-path can trigger renewal.
	fresh := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-b", clientJWTEntry{
		ByClientJWT: fresh,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// OOB control whose SendControl will get a 401 from the fake server.
	oob := connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "dead-jwt", ts.srv.URL)

	renewNow := make(chan struct{}, 1)
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-b",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            oob,
			RenewNow:       renewNow,
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	// Fire a SendControl that the fake server answers 401. The OOB's on401
	// callback (registered by the watcher) signals renewNow automatically —
	// this exercises the production fast-path, not a manual channel send.
	if err := ts.forceOob401(oob); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-b")
		if ok && entry.ByClientJWT != fresh {
			break // renewed
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew on 401 fast-path")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRenewsExpiredAtStartup(t *testing.T) {
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	// Already-expired token: a hot-restart that reused this would be a black
	// hole; the watcher must renew on its startup check, not the first tick.
	expired := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(-1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-c", clientJWTEntry{
		ByClientJWT: expired,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	renewNow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-c",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       renewNow,
			ProxyIndex:     0,
		})
	}()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-c")
		if ok && entry.ByClientJWT != expired {
			break // renewed by startup check
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew the already-expired token at startup")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherSkipsHealthyToken(t *testing.T) {
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	// Token expiring in 30h: far outside the 12h threshold, no 401s — the
	// watcher must NOT renew on a tick.
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-d", clientJWTEntry{
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	renewNow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-d",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       renewNow,
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(200 * time.Millisecond)

	entry, ok := store.Get("proxy-d")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != healthy {
		t.Fatalf("healthy token was renewed — watcher must skip tokens outside the 12h window")
	}
	cancel()
	<-done
}
