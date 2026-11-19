package gossip

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/network"
	ma "github.com/multiformats/go-multiaddr"
)

func TestNodeResourceLimitRecordsCap(t *testing.T) {
	limits := DefaultNodeResourceLimits(16)
	limits.ConnCount = 1
	limits.TransientConnCount = 1
	events := &EventRing{}
	manager, err := newNodeResourceManager(limits, events)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1234")
	conn, err := manager.OpenConnection(network.DirInbound, false, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Done()
	if extra, err := manager.OpenConnection(network.DirInbound, false, addr); err == nil {
		extra.Done()
		t.Fatal("extra connection was accepted")
	}
	got := events.Snapshot()
	if len(got) != 1 || got[0].Kind != "cap" || got[0].Reason != "resource_limit" {
		t.Fatalf("cap events = %+v", got)
	}
}
