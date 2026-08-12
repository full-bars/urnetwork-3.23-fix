package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRunProxyJWTWatcherMutexSerializesRenewals verifies the process-wide
// renewalMutex: with several watchers all due at once, at most one
// /network/auth-client request is in flight at any time (a box with 50-60
// proxies must not stampede the API).
func TestRunProxyJWTWatcherMutexSerializesRenewals(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 5
	ticks := make([]chan time.Time, n)
	done := make(chan struct{}, n)

	// All watchers share the package-level store (that's what the watcher
	// reads); each proxy gets a distinct identity key and its own tick
	// channel (a channel send delivers to only one receiver, so a shared
	// tick would wake just one watcher).
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	for i := 0; i < n; i++ {
		clientID := connect.NewId()
		key := "proxy-" + string(rune('a'+i))
		ticks[i] = make(chan time.Time, 1)
		expiring := createFakeJWTWithClaims(map[string]interface{}{
			"client_id": clientID.String(),
			"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
		})
		if err := store.Put(key, clientJWTEntry{
			ByClientJWT: expiring,
			ClientID:    clientID.String(),
			NetworkID:   "net-1",
			MintedAt:    time.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		go func(key string, cid connect.Id, tick chan time.Time) {
			defer func() { done <- struct{}{} }()
			runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
				IdentityKey:    key,
				ClientID:       cid,
				Description:    "test [beta-test]",
				ApiURL:         ts.srv.URL,
				ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
				OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
				RenewNow:       make(chan struct{}, 1),
				Tick:           tick,
				ProxyIndex:     i,
			})
		}(key, clientID, ticks[i])
	}

	// One simultaneous tick fires all five watchers at once; the mutex
	// serializes their /network/auth-client calls.
	for i := 0; i < n; i++ {
		ticks[i] <- time.Now()
	}

	// Poll the store until all five entries carry a fresh token (the watchers
	// renew synchronously in their tick handlers, but polling instead of
	// sleeping makes this robust under CI load). Then cancel so the watchers
	// exit and signal done.
	deadline := time.After(10 * time.Second)
	allRenewed := func() bool {
		for i := 0; i < n; i++ {
			key := "proxy-" + string(rune('a'+i))
			entry, ok := store.Get(key)
			if !ok {
				return false
			}
			if exp := parseJWTExpiryTime(entry.ByClientJWT); exp == nil || time.Until(*exp) < 20*time.Hour {
				return false
			}
		}
		return true
	}
	for !allRenewed() {
		select {
		case <-deadline:
			t.Fatal("not all watchers renewed their tokens")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()

	// Fresh deadline: time.After delivers exactly one value, and the polling
	// loop above may have consumed the first one — reusing it would let the
	// second loop block until the package timeout instead of failing fast.
	finishDeadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-finishDeadline:
			t.Fatal("watchers did not all finish")
		}
	}

	ts.assertMaxConcurrent(t, 1)
	ts.assertTotalRequests(t, n)
}

// TestRunProxyJWTWatcherKeepsOldTokenOnFailure verifies a failed renewal
// leaves the store untouched (old token still cached) so the hourly retry can
// try again — no data loss, no panic.
func TestRunProxyJWTWatcherKeepsOldTokenOnFailure(t *testing.T) {
	setTestHome(t)
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/network/auth-client" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer failSrv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	old := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy", clientJWTEntry{
		ByClientJWT: old,
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
			IdentityKey:    "proxy",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         failSrv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", failSrv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(200 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing after failed renewal")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed after failed renewal — must keep the old token for retry")
	}
	cancel()
	<-done
}
