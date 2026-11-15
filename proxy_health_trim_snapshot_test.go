package connect

import "testing"

func TestProxyHealthSnapshotExcludesOnlyHeldKeys(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	RegisterProxy(801, "a", "a:user")
	RegisterProxy(802, "b", "b:user")
	RegisterProxy(803, "c", "c:user")
	markProxyUp(801)
	markProxyUp(802)
	markProxyDown(802)
	markProxyUp(803)
	markProxyDown(803)
	RegisterProxyBandwidth(803).Clients.Store(2)
	up, dead, degraded, bw, connecting := ProxyHealthSnapshotExcluding(map[string]bool{"c:user": true})
	if up != 1 || len(dead) != 0 || len(degraded) != 1 || len(connecting) != 0 || len(bw) != 1 {
		t.Fatalf("up=%d dead=%v degraded=%v connecting=%v bandwidth=%d", up, dead, degraded, connecting, len(bw))
	}
	for _, b := range bw {
		if b.Clients.Load() != 2 {
			t.Fatal("held drain clients were lost")
		}
	}
}
