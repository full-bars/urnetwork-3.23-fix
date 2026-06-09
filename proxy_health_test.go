package connect

import "testing"

// resetProxyHealthForTest clears global registry state between tests.
func resetProxyHealthForTest() {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	proxyHealthByIndex = map[int]*proxyHealth{}
	proxyLifetimeRecovered = 0
	proxyLifetimeLost = 0
	proxyBaselineSet = false
}

func TestProxyHealthRegisterAndMark(t *testing.T) {
	resetProxyHealthForTest()

	RegisterProxy(0, "1.1.1.1:1081")
	RegisterProxy(1, "2.2.2.2:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// idempotent: re-register keeps the entry
	RegisterProxy(0, "1.1.1.1:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count after re-register = %d, want 2", got)
	}

	markProxyUp(0)
	proxyHealthMu.Lock()
	up := proxyHealthByIndex[0].currentlyUp
	ever := proxyHealthByIndex[0].everUp
	proxyHealthMu.Unlock()
	if !up || !ever {
		t.Fatalf("after markProxyUp: currentlyUp=%v everUp=%v, want true,true", up, ever)
	}

	markProxyDown(0)
	proxyHealthMu.Lock()
	up = proxyHealthByIndex[0].currentlyUp
	ever = proxyHealthByIndex[0].everUp
	downStamped := !proxyHealthByIndex[0].downSince.IsZero()
	proxyHealthMu.Unlock()
	if up || !ever || !downStamped {
		t.Fatalf("after markProxyDown: up=%v ever=%v downStamped=%v, want false,true,true", up, ever, downStamped)
	}

	// mark on unknown index is a no-op (must not panic)
	markProxyUp(999)
	markProxyDown(999)
}

func TestProxyHealthSnapshot(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(2, "c:1") // dead (never up)
	RegisterProxy(0, "a:1") // will be up
	RegisterProxy(1, "b:1") // will be degraded

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1) // up then down -> degraded

	up, dead, degraded, _, connecting := ProxyHealthSnapshot()
	if up != 1 {
		t.Fatalf("up = %d, want 1", up)
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want [] (RegisterProxy sets connecting=true)", dead)
	}
	if len(degraded) != 1 || degraded[0] != "proxy[1] (b:1)" {
		t.Fatalf("degraded = %v, want [proxy[1] (b:1)]", degraded)
	}
	if len(connecting) != 1 || connecting[0] != "proxy[2] (c:1)" {
		t.Fatalf("connecting = %v, want [proxy[2] (c:1)]", connecting)
	}

	// snapshot must NOT advance the baseline: lastSeenUp stays false everywhere
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	for idx, h := range proxyHealthByIndex {
		if h.lastSeenUp {
			t.Fatalf("snapshot advanced baseline for idx %d", idx)
		}
	}
}

func TestProxyHealthHeartbeatTransitions(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	RegisterProxy(1, "b:1") // becomes dead after up→down
	RegisterProxy(2, "c:1") // connecting (RegisterProxy sets connecting=true)

	// First call establishes the baseline: no transitions, no dead (confirmDead=false).
	r := ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 0 || len(r.NewlyDegraded) != 0 || len(r.NewlyDead) != 0 {
		t.Fatalf("first call should have no events, got %+v", r)
	}
	if r.LifetimeRecovered != 0 || r.LifetimeLost != 0 {
		t.Fatalf("first call lifetime counters should be 0, got %+v", r)
	}

	// Proxy 0 comes up -> recovered=1 (first-ever connect, after omitted).
	markProxyUp(0)
	r = ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 1 || r.Recovered[0].Index != 0 {
		t.Fatalf("Recovered = %+v, want [idx 0]", r.Recovered)
	}
	if r.LifetimeRecovered != 1 {
		t.Fatalf("LifetimeRecovered = %d, want 1", r.LifetimeRecovered)
	}

	// Proxy 1 comes up then drops -> NewlyDegraded=1, lifetime_lost=1.
	markProxyUp(1)
	r = ProxyHealthHeartbeat(false)
	markProxyDown(1)
	r = ProxyHealthHeartbeat(false)
	if len(r.NewlyDegraded) != 1 || r.NewlyDegraded[0].Index != 1 {
		t.Fatalf("NewlyDegraded = %+v, want [idx 1]", r.NewlyDegraded)
	}
	if r.LifetimeLost != 1 || r.LifetimeRecovered != 2 {
		t.Fatalf("lifetime = (rec %d, lost %d), want (2,1)", r.LifetimeRecovered, r.LifetimeLost)
	}

	// confirmDead=true: proxy 1 (was up, now down, everUp=true) is degraded, not dead.
	// proxy 2 (connecting, never up) is not dead either (still connecting).
	// No proxy matches the dead criteria (everUp=false, connecting=false).
	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 0 {
		t.Fatalf("NewlyDead = %+v, want [] (no dead proxies)", r.NewlyDead)
	}
	if len(r.Dead) != 0 {
		t.Fatalf("Dead = %+v, want []", r.Dead)
	}
}

func TestUnregisterProxy_RemovesFromRegistry(t *testing.T) {
	resetProxyHealthForTest()

	RegisterProxy(99, "1.2.3.4:1080")
	if ProxyHealthCount() != 1 {
		t.Fatal("expected 1 proxy registered")
	}

	UnregisterProxy(99)
	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("expected 0 proxies after unregister, got %d", got)
	}
}

func TestUnregisterProxy_NoopIfNotRegistered(t *testing.T) {
	resetProxyHealthForTest()

	// Must not panic
	UnregisterProxy(42)
}

func TestProxyHealthHeartbeatFlappingCountsTwice(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	ProxyHealthHeartbeat(false) // baseline

	markProxyUp(0)
	ProxyHealthHeartbeat(false) // recovered #1
	markProxyDown(0)
	ProxyHealthHeartbeat(false) // lost #1
	markProxyUp(0)
	r := ProxyHealthHeartbeat(false) // recovered #2

	if r.LifetimeRecovered != 2 {
		t.Fatalf("LifetimeRecovered = %d, want 2 (event semantics)", r.LifetimeRecovered)
	}
}
