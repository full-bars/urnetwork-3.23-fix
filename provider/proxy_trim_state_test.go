package main

import (
	"context"
	"os"
	"strings"
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
func trimFixture(t *testing.T) (*ProxyReloader, []string, *atomic.Int32) {
	return trimFixtureRunning(t, 3)
}

// trimFixtureRunning builds the same three-proxy source with only the first
// `running` proxies launched (in cancelMap), as after a capped startup.
func trimFixtureRunning(t *testing.T, running int) (*ProxyReloader, []string, *atomic.Int32) {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

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
		cancelMap:   map[string]context.CancelFunc{},
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
	for _, a := range addrs[:running] {
		r.cancelMap[a] = cancel
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })

	// reload() re-reads the persisted state at the start of every cycle, so the
	// fixture must be on disk, not just in r.state.
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	return r, addrs, &cancelled
}

func TestReload_TrimShedKeepsStateEntries(t *testing.T) {
	r, addrs, cancelled := trimFixture(t)
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

// The operator must see the command was received and what it did: one
// "received" line when the cap first appears and an "applied" line with the
// result, then silence on the reloads that follow with an unchanged cap, and a
// "received ... cleared" line when the cap is removed.
func TestReload_TrimLogsReceiptAndResult(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	r, _, _ := trimFixture(t)

	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	first := captureTlog(t, func() { r.reload() })
	if !strings.Contains(first, "[proxy][trim] received: cap=1 (was none); 3 running, 3 desired, applying") {
		t.Fatalf("missing receipt line, got:\n%s", first)
	}
	if !strings.Contains(first, "[proxy][trim] applied: cap=1: shed 2 worst-graded running") {
		t.Fatalf("missing result line, got:\n%s", first)
	}

	second := captureTlog(t, func() { r.reload() })
	if strings.Contains(second, "[proxy][trim] received") {
		t.Fatalf("unchanged cap must not be re-acknowledged, got:\n%s", second)
	}

	if err := writeTrimTarget(0); err != nil {
		t.Fatal(err)
	}
	cleared := captureTlog(t, func() { r.reload() })
	if !strings.Contains(cleared, "[proxy][trim] received: cap cleared (was 1)") {
		t.Fatalf("missing cleared line, got:\n%s", cleared)
	}
}

// After a capped startup only `cap` proxies are running and the rest are held.
// The startup reload must keep holding them (not launch them "as additions")
// while the cap stands, and must launch one once the cap is raised.
func TestReload_HeldProxiesStayHeldUntilTheCapRises(t *testing.T) {
	r, addrs, cancelled := trimFixtureRunning(t, 2)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}

	r.reload()
	if _, launched := r.cancelMap[addrs[2]]; launched {
		t.Fatalf("held proxy %s was launched while the cap of 2 is already met", addrs[2])
	}
	if got := cancelled.Load(); got != 0 {
		t.Fatalf("cap already met: nothing may be shed, cancelled %d", got)
	}
	if _, ok := r.state.Proxies[addrs[2]]; !ok {
		t.Fatalf("held proxy lost its state entry")
	}

	if err := writeTrimTarget(3); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if _, launched := r.cancelMap[addrs[2]]; !launched {
		t.Fatalf("with the cap raised to 3 the held proxy %s must be admitted", addrs[2])
	}
}
