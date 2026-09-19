package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestSameAuth distinguishes identical vs changed credentials — the decision
// function behind proxy credential rotation.
func TestSameAuth(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		a := &connect.ProxySettings{}
		b := &connect.ProxySettings{}
		if !sameAuth(a, b) {
			t.Fatal("two nil-auth settings must compare equal")
		}
	})
	t.Run("nil vs set is different", func(t *testing.T) {
		a := &connect.ProxySettings{}
		b := &connect.ProxySettings{Auth: &proxy.Auth{User: "u", Password: "p"}}
		if sameAuth(a, b) {
			t.Fatal("nil auth vs set auth must differ")
		}
	})
	t.Run("same credentials equal", func(t *testing.T) {
		a := &connect.ProxySettings{Auth: &proxy.Auth{User: "test-user", Password: "test-pass"}}
		b := &connect.ProxySettings{Auth: &proxy.Auth{User: "test-user", Password: "test-pass"}}
		if !sameAuth(a, b) {
			t.Fatal("identical credentials must compare equal")
		}
	})
	t.Run("credential rotation detected", func(t *testing.T) {
		oldCred := &connect.ProxySettings{Auth: &proxy.Auth{User: "test-user", Password: "old-pass"}}
		newCred := &connect.ProxySettings{Auth: &proxy.Auth{User: "test-user-rotated", Password: "new-pass"}}
		if sameAuth(oldCred, newCred) {
			t.Fatal("different credentials must not compare equal (LA7 paste incident)")
		}
	})
}

// TestSameAuth_TopLevelNilGuards verifies top-level nil pointer handling in sameAuth.
func TestSameAuth_TopLevelNilGuards(t *testing.T) {
	nonNil := &connect.ProxySettings{
		Auth: &proxy.Auth{User: "user", Password: "pass"},
	}

	t.Run("nil vs non-nil", func(t *testing.T) {
		if sameAuth(nil, nonNil) {
			t.Fatal("nil vs non-nil *ProxySettings must not compare equal")
		}
	})

	t.Run("non-nil vs nil", func(t *testing.T) {
		if sameAuth(nonNil, nil) {
			t.Fatal("non-nil vs nil *ProxySettings must not compare equal")
		}
	})

	t.Run("both nil", func(t *testing.T) {
		if !sameAuth(nil, nil) {
			t.Fatal("both nil (*ProxySettings) must compare equal")
		}
	})
}

// TestSameAuth_FieldLevelMismatches verifies edge cases where user or password differ individually.
func TestSameAuth_FieldLevelMismatches(t *testing.T) {
	base := &connect.ProxySettings{
		Auth: &proxy.Auth{User: "user", Password: "password"},
	}

	t.Run("set vs nil Auth", func(t *testing.T) {
		other := &connect.ProxySettings{}
		if sameAuth(base, other) {
			t.Fatal("set Auth vs nil Auth must not compare equal")
		}
	})

	t.Run("same user different password", func(t *testing.T) {
		other := &connect.ProxySettings{
			Auth: &proxy.Auth{User: "user", Password: "different-password"},
		}
		if sameAuth(base, other) {
			t.Fatal("same user with different password must not compare equal")
		}
	})

	t.Run("different user same password", func(t *testing.T) {
		other := &connect.ProxySettings{
			Auth: &proxy.Auth{User: "different-user", Password: "password"},
		}
		if sameAuth(base, other) {
			t.Fatal("different user with same password must not compare equal")
		}
	})
}

// writeProxyConfigForTest writes a Servers map into the state dir, mirroring
// how paste/add persist entries keyed host:port:user:pass.
func writeProxyConfigForTest(t *testing.T, dir string, servers map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := readProxyConfig()
	if cfg.Servers == nil {
		cfg.Servers = map[string]string{}
	}
	for k, v := range servers {
		cfg.Servers[k] = v
	}
	writeProxyConfig(cfg)
}

// TestProxyAddRotatesCredentials verifies proxyAdd removes an existing
// same-address entry with different credentials when a new one is added, so a
// re-paste becomes a rotation instead of a silent duplicate that the
// address-keyed reload diff ignores (LA7 incident).
func TestProxyAddRotatesCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	resetReloadTriggerForTest(t)

	writeProxyConfigForTest(t, filepath.Join(dir, ".urnetwork"), map[string]string{
		"192.0.2.4:1080:olduser:oldpass": "",
		"192.0.2.9:1080":                 "",
	})

	opts := docopt.Opts{
		"<key_address>": []string{"192.0.2.4:1080:newuser:newpass"},
		"-f":            true,
	}
	proxyAdd(opts)

	got := readProxyConfig()
	if len(got.Servers) != 2 {
		t.Fatalf("want 2 servers after rotation (replaced 1, kept 1), got %d: %v", len(got.Servers), got.Servers)
	}
	if _, ok := got.Servers["192.0.2.4:1080:newuser:newpass"]; !ok {
		t.Fatalf("new credential entry missing: %v", got.Servers)
	}
	if _, ok := got.Servers["192.0.2.4:1080:olduser:oldpass"]; ok {
		t.Fatalf("old credential entry was not rotated away: %v", got.Servers)
	}
	if _, ok := got.Servers["192.0.2.9:1080"]; !ok {
		t.Fatalf("unrelated proxy must survive: %v", got.Servers)
	}
}

// TestProxyAddMultiProxyInlineCreds is the critical regression test for
// the fleet-wide credential clobber bug. When operators paste multiple
// proxies with different inline credentials in a single call, each proxy
// must retain its own credentials — none should be overwritten by another.
func TestProxyAddMultiProxyInlineCreds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	opts := docopt.Opts{
		"<key_address>": []string{
			"192.0.2.1:1080:userA:passA",
			"192.0.2.2:1080:userB:passB",
			"192.0.2.3:1080",
		},
		"-f": true,
	}
	proxyAdd(opts)

	settings := readProxySettings()
	if len(settings) != 3 {
		t.Fatalf("expected 3 settings returned, got %d", len(settings))
	}

	byAddr := make(map[string]*connect.ProxySettings, len(settings))
	for _, s := range settings {
		byAddr[s.Address] = s
	}

	s1, ok := byAddr["192.0.2.1:1080"]
	if !ok {
		t.Fatal("missing setting for 192.0.2.1:1080")
	}
	if s1.Auth == nil || s1.Auth.User != "userA" || s1.Auth.Password != "passA" {
		t.Errorf("192.0.2.1:1080 should have userA/passA credentials, got %v", s1.Auth)
	}

	s2, ok := byAddr["192.0.2.2:1080"]
	if !ok {
		t.Fatal("missing setting for 192.0.2.2:1080")
	}
	if s2.Auth == nil || s2.Auth.User != "userB" || s2.Auth.Password != "passB" {
		t.Errorf("192.0.2.2:1080 should have userB/passB credentials, got %v", s2.Auth)
	}

	s3, ok := byAddr["192.0.2.3:1080"]
	if !ok {
		t.Fatal("missing setting for 192.0.2.3:1080")
	}
	if s3.Auth != nil {
		t.Errorf("192.0.2.3:1080 should have no credentials, got %v", s3.Auth)
	}
}

// TestProxyReloader_ReloadRotationExecution verifies that reload() actively
// cancels a running proxy and spawns a new one when auth changes in the config.
// The rotated proxy must appear in both the removed set (cancels the old
// goroutine) and the added set (relaunches with new auth) in a single pass.
func TestProxyReloader_ReloadRotationExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "192.0.2.4:1080"
	oldSettings := &connect.ProxySettings{
		Network: "tcp",
		Address: proxyAddr,
		Auth: &proxy.Auth{
			User:     "olduser",
			Password: "oldpass",
		},
	}

	// Write new credentials to config
	cfg := ProxyConfig{
		Servers: map[string]string{
			fmt.Sprintf("%s:newuser:newpass", proxyAddr): "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := make(chan struct{}, 1)
	oldCancel := func() {
		select {
		case cancelCalled <- struct{}{}:
		default:
		}
	}

	spawnedChan := make(chan *connect.ProxySettings, 1)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	spawnProxy := func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		spawnedChan <- settings
		<-proxyCtx.Done()
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: oldCancel,
		},
		cancelMapMu: cancelMapMu,
		runningAuth: map[string]*connect.ProxySettings{
			proxyAddr: oldSettings,
		},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       parentCtx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	// 1. Verify old cancel function was called
	select {
	case <-cancelCalled:
	case <-time.After(1 * time.Second):
		t.Fatal("expected old proxy cancel function to be called on credential rotation")
	}

	// 2. Verify new proxy was spawned with new credentials
	select {
	case spawned := <-spawnedChan:
		if spawned.Address != proxyAddr {
			t.Fatalf("expected spawned address %s, got %s", proxyAddr, spawned.Address)
		}
		if spawned.Auth == nil {
			t.Fatal("expected spawned proxy to have Auth credentials, got nil")
		}
		if spawned.Auth.User != "newuser" || spawned.Auth.Password != "newpass" {
			t.Fatalf("expected spawned credentials newuser/newpass, got %s/%s", spawned.Auth.User, spawned.Auth.Password)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spawnProxy callback on credential rotation")
	}

	// 3. Verify runningAuth was updated to new credentials
	auth, ok := reloader.runningAuthFor(proxyAddr)
	if !ok {
		t.Fatalf("runningAuth missing entry for %s after rotation", proxyAddr)
	}
	if auth.Auth == nil || auth.Auth.User != "newuser" || auth.Auth.Password != "newpass" {
		t.Fatalf("expected runningAuth updated to newuser/newpass, got %v", auth.Auth)
	}
}

// TestStartupRunningAuthSeeding verifies that seeding runningAuth at startup
// prevents reload() from churning running proxies when the configuration matches.
func TestStartupRunningAuthSeeding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyKey := "192.0.2.4:1080:user:pass"
	cfg := ProxyConfig{
		Servers: map[string]string{proxyKey: ""},
	}
	writeProxyConfig(&cfg)

	settingsList := readProxySettings()
	if len(settingsList) != 1 {
		t.Fatalf("expected 1 proxy setting, got %d", len(settingsList))
	}
	seededSetting := settingsList[0]

	cancelFunc := func() {
		t.Fatal("cancel must not be invoked when runningAuth is seeded at startup")
	}

	spawnProxy := func(ctx context.Context, s *connect.ProxySettings, isNative bool, isURLSourced bool) {
		t.Fatalf("spawnProxy must not be invoked when runningAuth matches, got: %v", s)
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			seededSetting.Address: cancelFunc,
		},
		cancelMapMu:     cancelMapMu,
		runningAuth:     map[string]*connect.ProxySettings{seededSetting.Address: seededSetting},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       parentCtx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	// Immediate reload should be a no-op (same auth, same address)
	reloader.reload()

	// Verify the proxy remains in cancelMap and runningAuth
	cancelMapMu.Lock()
	_, stillRunning := reloader.cancelMap[seededSetting.Address]
	cancelMapMu.Unlock()
	if !stillRunning {
		t.Fatal("proxy was unexpectedly removed from cancelMap")
	}

	auth, ok := reloader.runningAuthFor(seededSetting.Address)
	if !ok || !sameAuth(auth, seededSetting) {
		t.Fatal("runningAuth entry altered or missing after reload")
	}
}

// TestProxyReloader_UnrecordedAuthTriggersRotation verifies that an address present
// in cancelMap but absent in runningAuth triggers rotation on reload.
func TestProxyReloader_UnrecordedAuthTriggersRotation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "192.0.2.4:1080"
	cfg := ProxyConfig{
		Servers: map[string]string{
			fmt.Sprintf("%s:u:p", proxyAddr): "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := false
	oldCancel := func() {
		cancelCalled = true
	}

	spawnedChan := make(chan *connect.ProxySettings, 1)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	spawnProxy := func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		spawnedChan <- settings
		<-proxyCtx.Done()
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: oldCancel,
		},
		cancelMapMu:     cancelMapMu,
		runningAuth:     map[string]*connect.ProxySettings{}, // empty: proxy was unrecorded
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       parentCtx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	if !cancelCalled {
		t.Fatal("expected unrecorded proxy to be cancelled on reload")
	}
	select {
	case spawned := <-spawnedChan:
		if spawned.Address != proxyAddr {
			t.Fatalf("expected spawned address %s, got %s", proxyAddr, spawned.Address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spawnProxy")
	}
}

// TestProxyReloader_RemovedProxyCleansUpRunningAuth verifies that removing a proxy
// cancels it and drops its entry from runningAuth.
func TestProxyReloader_RemovedProxyCleansUpRunningAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "192.0.2.4:1080"
	// Keep one proxy so desired set is non-empty (reload won't early-return on empty)
	keptAddr := "192.0.2.9:1080"
	cfg := ProxyConfig{
		Servers: map[string]string{
			keptAddr: "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := false
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: func() { cancelCalled = true },
			keptAddr:  func() {},
		},
		cancelMapMu: &sync.Mutex{},
		runningAuth: map[string]*connect.ProxySettings{
			proxyAddr: {Address: proxyAddr},
			keptAddr:  {Address: keptAddr},
		},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       context.Background(),
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(ctx context.Context, s *connect.ProxySettings, n bool, u bool) {},
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	if !cancelCalled {
		t.Fatal("expected removed proxy cancel function to be called")
	}
	if _, ok := reloader.runningAuthFor(proxyAddr); ok {
		t.Fatal("expected runningAuth entry to be purged for removed proxy")
	}
}

// An existing entry whose credentials come from the Auths table is the same
// credential as an inline user:pass form of the same address; adding it is not
// a rotation and must not purge the existing mapping.
func TestProxyAddKeepsEntryWithSameEffectiveCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	resetReloadTriggerForTest(t)

	cfg := readProxyConfig()
	cfg.Servers = map[string]string{"192.0.2.4:1080": "k1"}
	cfg.Auths = map[string]*ProxyAuth{"k1": {User: "alice", Password: "secret"}}
	if err := os.MkdirAll(filepath.Join(dir, ".urnetwork"), 0700); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(cfg)

	proxyAdd(docopt.Opts{
		"<key_address>": []string{"192.0.2.4:1080:alice:secret"},
		"-f":            true,
	})

	got := readProxyConfig()
	if _, ok := got.Servers["192.0.2.4:1080"]; !ok {
		t.Fatalf("entry with the same effective credentials was purged: %v", got.Servers)
	}
}

// resetReloadTriggerForTest isolates the process-global reload-trigger
// debounce: proxyAdd writes the trigger, and a trailing time.AfterFunc from
// one test must not recreate files under another test's HOME or suppress its
// trigger writes.
func resetReloadTriggerForTest(t *testing.T) {
	t.Helper()
	lastReloadTriggerTime.Lock()
	oldDebounce := writeReloadTriggerDebounce
	oldTS, oldPending := lastReloadTriggerTime.ts, lastReloadTriggerTime.pending
	writeReloadTriggerDebounce = 0
	lastReloadTriggerTime.ts = time.Time{}
	lastReloadTriggerTime.pending = false
	lastReloadTriggerTime.Unlock()
	t.Cleanup(func() {
		lastReloadTriggerTime.Lock()
		writeReloadTriggerDebounce = oldDebounce
		lastReloadTriggerTime.ts, lastReloadTriggerTime.pending = oldTS, oldPending
		lastReloadTriggerTime.Unlock()
	})
}
