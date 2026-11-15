package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestTrimHeldDrainKeepsClients(t *testing.T) {
	resetTrimHeld()
	t.Cleanup(resetTrimHeld)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	t.Setenv("DISABLE_DIRECT_IP", "1")
	r, keys, _ := trimFixture(t)
	connect.RegisterProxy(9010, "draining", keys[1])
	connect.RegisterProxyBandwidth(9010).Clients.Store(4)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if !r.isDraining(keys[1]) || !trimHeldSnapshot()[keys[1]] {
		t.Fatal("trim-shed proxy should be held while draining")
	}
	counts, clients := productionSnapshotSources().pool()
	if counts.Parked != 1 || counts.Connecting != 0 || clients != 4 {
		t.Fatalf("draining held proxy: %+v clients=%d", counts, clients)
	}
}
