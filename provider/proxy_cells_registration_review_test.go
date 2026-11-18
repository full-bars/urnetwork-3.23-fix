package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyCellStoreRegistrationRebase(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()
	now := time.Unix(1000, 0)
	store := &proxyCellStore{prev: map[int]uint64{}}
	connect.RegisterProxy(1, "same", "first")
	bw := connect.RegisterProxyBandwidth(1)
	bw.TotalRx.Store(100)
	store.Detail(now)
	connect.RegisterProxy(1, "same", "second")
	bw.TotalRx.Store(200)
	if got := store.Detail(now.Add(time.Second))[0].IntervalBytes; got != 0 {
		t.Fatalf("new registration inherited interval %d", got)
	}
	bw.TotalRx.Store(250)
	if got := store.Detail(now.Add(2 * time.Second))[0].IntervalBytes; got != 50 {
		t.Fatalf("same registration interval %d, want 50", got)
	}
	connect.RegisterProxy(1, "same", "first")
	connect.RegisterProxy(1, "same", "second")
	bw.TotalRx.Store(300)
	if got := store.Detail(now.Add(3 * time.Second))[0].IntervalBytes; got != 0 {
		t.Fatalf("unobserved round-trip registration inherited interval %d", got)
	}
	connect.UnregisterProxy(1)
	connect.RegisterProxy(1, "same", "second")
	connect.RegisterProxyBandwidth(1).TotalRx.Store(500)
	if got := store.Detail(now.Add(4 * time.Second))[0].IntervalBytes; got != 0 {
		t.Fatalf("recreated registration inherited interval %d", got)
	}
}
