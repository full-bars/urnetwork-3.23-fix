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
	settings := []*connect.ProxySettings{{Address: "a:1080"}, {Address: "b:1080"}, {Address: "c:1080"}}
	launch, held := startupTrimSelection(settings, 1, map[string]ProxyEntry{}, func(string) (float64, bool) { return 0, false }, func(string) float64 { return 0 })
	startupHeld(held)
	if len(launch) != 1 || len(held) != 2 || len(trimHeldSnapshot()) != 2 {
		t.Fatalf("startup selection: launch=%d held=%d published=%d", len(launch), len(held), len(trimHeldSnapshot()))
	}
	for _, s := range held {
		if !trimHeldSnapshot()[s.Key()] {
			t.Fatalf("startup-held identity %s missing", s.Key())
		}
	}
	if trimHeldSnapshot()[launch[0].Key()] {
		t.Fatal("startup launch incorrectly marked held")
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
