package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestTrimHeldSnapshotLifecycle(t *testing.T) {
	resetTrimHeld()
	t.Cleanup(resetTrimHeld)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	t.Setenv("DISABLE_DIRECT_IP", "1")
	r, keys, _ := trimFixtureRunning(t, 2)
	connect.RegisterProxy(9001, "a", keys[0])
	connect.RegisterProxy(9002, "b", keys[1])
	connect.RegisterProxy(9003, "c", keys[2])
	connect.RegisterProxyBandwidth(9003).Clients.Store(2)
	startupHeld([]*connect.ProxySettings{{Address: "unused"}})
	setTrimHeld("unused", false)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if !trimHeldSnapshot()[keys[2]] {
		t.Fatal("reload did not mark the held addition")
	}
	counts, clients := productionSnapshotSources().pool()
	if counts.Parked != 1 || counts.Connecting != 2 || clients != 2 {
		t.Fatalf("held proxy should be excluded from health, not drain clients: %+v clients=%d", counts, clients)
	}
	if err := writeTrimTarget(3); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if trimHeldSnapshot()[keys[2]] {
		t.Fatal("readmitted proxy remains parked")
	}
}

func TestTrimHeldStartupAndShed(t *testing.T) {
	resetTrimHeld()
	t.Cleanup(resetTrimHeld)
	s := &connect.ProxySettings{Address: "example:1080"}
	startupHeld([]*connect.ProxySettings{s})
	if !trimHeldSnapshot()[s.Key()] {
		t.Fatal("startup-held identity missing")
	}
	resetTrimHeld()
	r, keys, _ := trimFixture(t)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if !trimHeldSnapshot()[keys[1]] {
		t.Fatal("trim shed identity missing")
	}
	r.clearTrimShed(keys[1])
	if trimHeldSnapshot()[keys[1]] {
		t.Fatal("shed identity not cleared")
	}
}
