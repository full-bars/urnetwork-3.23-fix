package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// A trim shed cancels the proxy but must NOT forget it: the trim code and the
// prune pass both say "do not erase grade/health history", so the removal loop
// must keep the state entry of a trim-shed proxy. Dropping it made the proxy
// relaunch (once the cap was raised) under a brand new ID, ungraded, having
// lost its health and downtime history.
func TestReload_TrimShedKeepsStateEntries(t *testing.T) {
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	// globalClientJWTStore is built at package init from the REAL home, before
	// withTempHome can redirect HOME, so reload would read the developer's
	// stored identities and write snapshot files into their real state dir.
	prevStore := globalClientJWTStore
	globalClientJWTStore = newClientJWTStore(t.TempDir() + "/.client_jwts.json")
	t.Cleanup(func() { globalClientJWTStore = prevStore })

	// Credentialed proxies, so the state key is the identity key (addr+user).
	addrs := make([]string, 3)
	baseline := map[string]*connect.ProxySettings{}
	lines := ""
	for i, host := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		a := host + ":1080"
		s := &connect.ProxySettings{Address: a, Auth: &proxy.Auth{User: "u", Password: "p"}}
		addrs[i] = s.Key()
		baseline[s.Key()] = s // recorded auth, as the startup loop seeds it
		lines += a + ":u:p\n"
	}
	file := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(file, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Int32
	cancel := context.CancelFunc(func() { cancelled.Add(1) })
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			addrs[0]: cancel,
			addrs[1]: cancel,
			addrs[2]: cancel,
		},
		cancelMapMu: &sync.Mutex{},
		runningAuth: baseline,
		state: &ProxyState{Proxies: map[string]ProxyEntry{
			addrs[0]: {ID: 1, Health: "up", Graded: true, Score: 0.9},
			addrs[1]: {ID: 2, Health: "dead", Graded: true, Score: 0.2},
			addrs[2]: {ID: 3, Health: "offline", Graded: true, Score: 0.4},
		}},
		sourcePath:      file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) { <-parent.Done() },
		drainingProxies: map[string]context.CancelFunc{},
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })

	// reload() re-reads the persisted state at the start of every cycle, so the
	// fixture must be on disk, not just in r.state.
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	r.reload()

	if got := cancelled.Load(); got != 2 {
		t.Fatalf("trim to 1 of 3 must cancel exactly 2 proxies, cancelled %d", got)
	}
	for _, shed := range []string{addrs[1], addrs[2]} {
		e, ok := r.state.Proxies[shed]
		if !ok {
			t.Fatalf("trim-shed proxy %s lost its state entry (ID, health, grade history)", shed)
		}
		if !e.Graded || e.Health == "" {
			t.Fatalf("trim-shed proxy %s state entry was mutated: %+v", shed, e)
		}
	}
	if _, ok := r.state.Proxies[addrs[0]]; !ok {
		t.Fatalf("surviving proxy %s lost its state entry", addrs[0])
	}
}
