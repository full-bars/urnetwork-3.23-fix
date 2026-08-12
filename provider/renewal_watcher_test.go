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

	// Fire a SendControl that the fake server answers 401 -> counter goes to 1.
	if err := ts.forceOob401(oob); err != nil {
		t.Fatal(err)
	}
	// Deliver the renew-now signal.
	renewNow <- struct{}{}

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
