package connect

import (
	"testing"
	"time"
)

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

func TestDegradedProxies_EmptyWhenAllUp(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	markProxyUp(0)

	dps := DegradedProxies()
	if len(dps) != 0 {
		t.Fatalf("DegradedProxies = %d, want 0 (all up)", len(dps))
	}
}

func TestDegradedProxies_IncludesDegradedOnly(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1") // up
	RegisterProxy(1, "b:1") // degraded
	RegisterProxy(2, "c:1") // connecting (never up, never down)

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].Address != "b:1" {
		t.Fatalf("degraded address = %q, want %q", dps[0].Address, "b:1")
	}
	if dps[0].DownFor <= 0 {
		t.Fatal("DownFor should be positive")
	}
}

func TestDegradedProxies_ExcludesConnectingAndDead(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1") // connecting (never up)
	RegisterProxy(1, "b:1") // dead (never up, connecting=false)

	// Set proxy 1 to dead: it was registered with connecting=true,
	// set connecting=false without ever marking up
	proxyHealthMu.Lock()
	if h, ok := proxyHealthByIndex[1]; ok {
		h.connecting = false
	}
	proxyHealthMu.Unlock()

	dps := DegradedProxies()
	if len(dps) != 0 {
		t.Fatalf("DegradedProxies = %d, want 0 (no everUp proxies)", len(dps))
	}
}

func TestDegradedProxies_BandwidthPopulated(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	markProxyUp(0)

	bw := RegisterProxyBandwidth(0)
	bw.TotalRx.Store(1000)
	bw.TotalTx.Store(2000)

	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].TotalRxBytes != 1000 {
		t.Fatalf("TotalRxBytes = %d, want 1000", dps[0].TotalRxBytes)
	}
	if dps[0].TotalTxBytes != 2000 {
		t.Fatalf("TotalTxBytes = %d, want 2000", dps[0].TotalTxBytes)
	}
}

func TestDegradedProxies_NilBandwidth(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	markProxyUp(0)
	markProxyDown(0)

	// Don't call RegisterProxyBandwidth — bw stays nil
	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].TotalRxBytes != 0 || dps[0].TotalTxBytes != 0 {
		t.Fatalf("expected zero bandwidth when bw is nil, got rx=%d tx=%d",
			dps[0].TotalRxBytes, dps[0].TotalTxBytes)
	}
}

func TestDegradedProxies_DownSinceStamped(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1")
	markProxyUp(0)
	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].DownFor <= 0 {
		t.Fatal("DownFor should be positive after markProxyDown")
	}
}

func TestDegradedProxies_NotSorted(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1") // will be degraded last
	RegisterProxy(1, "b:1") // will be degraded first

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1)

	time.Sleep(time.Millisecond)

	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 2 {
		t.Fatalf("DegradedProxies = %d, want 2", len(dps))
	}
	// Should NOT be sorted — order is from map iteration
	// Just verify both addresses are present
	addrs := map[string]bool{}
	for _, d := range dps {
		addrs[d.Address] = true
	}
	if !addrs["a:1"] || !addrs["b:1"] {
		t.Fatal("both degraded addresses should be present")
	}
}

func TestIsDegraded_TrueWhenDegraded(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(10, "degraded:1")
	markProxyUp(10)
	markProxyDown(10)

	if !IsDegraded("degraded:1") {
		t.Fatal("expected degraded:1 to be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenCurrentlyUp(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(11, "up:1")
	markProxyUp(11)

	if IsDegraded("up:1") {
		t.Fatal("expected up:1 (currentlyUp) to not be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenUnregistered(t *testing.T) {
	resetProxyHealthForTest()

	if IsDegraded("nonexistent:1") {
		t.Fatal("expected an unregistered address to not be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenRecoveredAfterDown(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(12, "recovered:1")
	markProxyUp(12)
	markProxyDown(12)

	if !IsDegraded("recovered:1") {
		t.Fatal("expected recovered:1 to be degraded before recovery")
	}

	markProxyUp(12)
	if IsDegraded("recovered:1") {
		t.Fatal("expected recovered:1 to no longer be degraded after reconnecting")
	}
}

func TestIsDegraded_FalseWhileRespawnConnecting(t *testing.T) {
	// This is the scenario CodeRabbit flagged: RegisterProxy reuses the
	// existing *proxyHealth struct for an index rather than resetting it, so
	// a freshly respawned instance inherits its predecessor's stale everUp/
	// downSince fields. Without the `connecting` check, IsDegraded would
	// report a brand-new instance as "degraded" before it had ever attempted
	// to connect — exactly the gap that let the reaper cancel a replacement
	// instance instead of the stuck one a decision was actually made about.
	resetProxyHealthForTest()
	RegisterProxy(13, "respawn:1")
	markProxyUp(13)
	markProxyDown(13)

	if !IsDegraded("respawn:1") {
		t.Fatal("expected respawn:1 to be degraded before respawn")
	}

	// Simulate hot-reload tearing down and respawning a fresh instance at
	// the same index/address: RegisterProxy sets connecting=true and reuses
	// the struct, leaving everUp/downSince stale until the new instance
	// reports its own first transition.
	RegisterProxy(13, "respawn:1")

	if IsDegraded("respawn:1") {
		t.Fatal("expected respawn:1 to not be degraded while the respawned instance is still connecting")
	}
}

func TestDegradedProxiesExcludesConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(20, "degraded-conn:1")
	markProxyUp(20)
	markProxyDown(20)

	found := false
	for _, e := range DegradedProxies() {
		if e.Index == 20 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected index 20 to appear in DegradedProxies before respawn")
	}

	// Respawn at the same index: RegisterProxy sets connecting=true and reuses
	// the struct, so the stale everUp/downSince must not make it read degraded.
	RegisterProxy(20, "degraded-conn:1")
	for _, e := range DegradedProxies() {
		if e.Index == 20 {
			t.Fatal("expected index 20 to be excluded from DegradedProxies while connecting")
		}
	}
}

func TestProxyHealthByAddressReportsConnectingOnRespawn(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(21, "respawn-addr:2")
	markProxyUp(21)
	markProxyDown(21)

	status := ProxyHealthByAddress()
	if s, ok := status["respawn-addr:2"]; !ok || s.Health != "recently_offline" {
		t.Fatalf("expected recently_offline before respawn, got %q", s.Health)
	}

	RegisterProxy(21, "respawn-addr:2")
	status = ProxyHealthByAddress()
	if s, ok := status["respawn-addr:2"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting after respawn, got %q", s.Health)
	}
}

func TestProxyHealthByAddressUpWinsOverConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(22, "up-wins:3")
	markProxyUp(22)

	// Re-register while still up: connecting=true, but currentlyUp must win.
	RegisterProxy(22, "up-wins:3")
	status := ProxyHealthByAddress()
	if s, ok := status["up-wins:3"]; !ok || s.Health != "up" {
		t.Fatalf("expected up to win over connecting, got %q", s.Health)
	}
}
