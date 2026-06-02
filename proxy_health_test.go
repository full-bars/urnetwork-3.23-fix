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
