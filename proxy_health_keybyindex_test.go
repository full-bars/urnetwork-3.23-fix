package connect

import "testing"

// ProxyKeyByIndex resolves a registry index to the identity key it was
// registered under, for consumers that only hold the "proxy[N] (addr)" display
// string and need the proxy.state key.
func TestProxyKeyByIndex(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)

	RegisterProxy(3, "10.0.0.1:1080", "10.0.0.1:1080\x1fu")
	if got := ProxyKeyByIndex(3); got != "10.0.0.1:1080\x1fu" {
		t.Fatalf("ProxyKeyByIndex(3)=%q, want the identity key", got)
	}
	if got := ProxyKeyByIndex(99); got != "" {
		t.Fatalf("unregistered index must return empty key, got %q", got)
	}
}
