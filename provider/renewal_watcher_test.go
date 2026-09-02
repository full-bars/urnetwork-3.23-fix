package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// setTestHome points HOME at a temp dir containing a fake account JWT, so
// readAccountJWT() (used by the renewal watcher) resolves inside the test and
// never touches the developer's real ~/.urnetwork/jwt. Returns the temp dir.
func setTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// Account JWT: any token with a network_id claim is enough — the fake
	// auth-client server ignores the Bearer and issues a fresh client JWT.
	accountJwt := createFakeJWTWithClaims(map[string]interface{}{
		"network_id": "net-1",
		"exp":        float64(time.Now().Add(48 * time.Hour).Unix()),
	})
	urnetworkDir := filepath.Join(dir, ".urnetwork")
	if err := os.MkdirAll(urnetworkDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(urnetworkDir, "jwt"), []byte(accountJwt), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunProxyJWTWatcherRenewsOnExpiry(t *testing.T) {
	setTestHome(t)
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
	setTestHome(t)
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
	setTestHome(t)
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
	setTestHome(t)
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

// TestRunProxyJWTWatcherKeepsOldTokenOnClientLimitExceeded pins the
// HTTP-200-with-result.Error path (ClientLimitExceeded): the watcher must
// keep the old token so the hourly retry can try again.
func TestRunProxyJWTWatcherKeepsOldTokenOnClientLimitExceeded(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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

	// Script the server-side rejection: HTTP 200 body carrying result.Error.
	ts.scriptedResponse.Store(&connect.AuthNetworkClientResult{
		Error: &connect.AuthNetworkClientError{
			ClientLimitExceeded: true,
			Message:             "Client limit exceeded.",
		},
	})

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing after ClientLimitExceeded renewal failure")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed after ClientLimitExceeded — must keep the old token for retry")
	}
	cancel()
	<-done
}

// TestRunProxyJWTWatcherMissingAccountJWTDoesNotPanic pins the readAccountJWT
// failure path: no ~/.urnetwork/jwt on disk → the watcher logs and skips,
// leaving the store untouched and the process alive.
func TestRunProxyJWTWatcherMissingAccountJWTDoesNotPanic(t *testing.T) {
	// HOME points at an EMPTY temp dir (no .urnetwork/jwt).
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("USERPROFILE", emptyHome)

	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed — watcher must not renew without an account JWT")
	}
	cancel()
	<-done
}

// TestRunProxyJWTWatcherRejectsJwtMissingClientId pins the regression guard:
// a renewal response whose JWT lacks a client_id claim must be rejected and
// the old token kept.
func TestRunProxyJWTWatcherRejectsJwtMissingClientId(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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

	// The fake server omits the client_id claim from the returned JWT.
	ts.omitClientIdClaim.Store(true)

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed — renewal returning a JWT without client_id must be rejected")
	}
	cancel()
	<-done
}

// TestRunProxyJWTWatcherRetriesOnStorePutFailure pins the HIGH-2 finding: a
// renewal whose store write fails must NOT reset the 401 counter, so the next
// 401 re-triggers renewal instead of silently accepting the in-memory swap.
//
// Setup: the global store points at an unwritable path (Put always fails),
// so the ONLY renewal trigger available is the 401 counter — a clean
// isolation of the "counter must stay armed on persistence failure" behavior.
// waitForRenewalRequest polls until the auth-client endpoint has handled at
// least one MORE request than `baseline`, failing after a bounded deadline.
// Polling replaces the old fixed `time.Sleep(300ms)` waits that flaked under
// full-suite CPU contention: the watcher's renewal goroutine occasionally took
// longer than 300ms, so totalRequests was read before the renewal landed and
// the next assert tripped. The condition is "count grew past the snapshot", so
// it stays correct even if a single renewal issues multiple auth-client calls.
func (ts *renewalTestServer) waitForRenewalRequest(t *testing.T, baseline int32) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		if ts.totalRequests.Load() > baseline {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("watcher did not make a renewal request: total auth-client requests = %d, baseline = %d", ts.totalRequests.Load(), baseline)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestRunProxyJWTWatcherRetriesOnStorePutFailure(t *testing.T) {
	// Read-only dir does not block writes for UID 0 (CI images often run as
	// root, bypassing mode bits) — the Put-failure premise doesn't hold
	// there, so skip rather than fail confusingly.
	if os.Geteuid() == 0 {
		t.Skip("store-write-failure premise requires non-root (mode bits bypassed by UID 0)")
	}
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	// Unwritable store: a read-only parent dir → Put's WriteFile fails on
	// every call (flushLocked's MkdirAll succeeds, then WriteFile is denied).
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0700) })
	brokenStore := newClientJWTStore(roDir + "/client_jwts.json")
	oldStore := globalClientJWTStore
	globalClientJWTStore = brokenStore
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := connect.NewApiOutOfBandControl(ctx, connect.NewClientStrategyWithDefaults(ctx), "dead-jwt", ts.srv.URL)
	renewNow := make(chan struct{}, 1)
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy",
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

	// Fire one 401 through the OOB → counter becomes 1.
	if err := ts.forceOob401(oob); err != nil {
		t.Fatal(err)
	}
	// First renewNow: renewal runs, Put fails → counter must stay armed. Wait
	// (adaptively) for the endpoint hit instead of a fixed sleep.
	baseline := ts.totalRequests.Load()
	renewNow <- struct{}{}
	ts.waitForRenewalRequest(t, baseline)
	first := ts.totalRequests.Load()

	// Second renewNow: if the counter had been reset, nothing would fire
	// (the exp check is disabled — currentJwt is empty on a broken store).
	baseline = ts.totalRequests.Load()
	renewNow <- struct{}{}
	ts.waitForRenewalRequest(t, baseline)
	second := ts.totalRequests.Load()
	if second <= first {
		t.Fatalf("401 counter was reset after a failed store write: requests %d -> %d — next 401 must re-trigger renewal", first, second)
	}
	cancel()
	<-done
}
