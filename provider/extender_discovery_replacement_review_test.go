package main

import (
	"testing"
	"time"
)

func TestExtenderDiscoveryLogResetsOnRoleReplacement(t *testing.T) {
	first, second := &extenderRole{}, &extenderRole{}
	m := &extenderManager{role: first}
	now := time.Unix(1000, 0)
	m.logDiscoveryHealth(now)
	m.discoveryLog.hadPeers = true
	m.logDiscoveryHealth(now.Add(time.Second))
	if !m.discoveryLog.hadPeers {
		t.Fatal("same role unexpectedly reset")
	}
	m.role = second // no tick observes nil between the two roles
	m.logDiscoveryHealth(now.Add(2 * time.Second))
	if m.discoveryLog.hadPeers {
		t.Fatal("replacement retained prior role discovery state")
	}
	m.discoveryLog.hadPeers = true
	m.role = nil
	m.logDiscoveryHealth(now.Add(3 * time.Second))
	if m.discoveryLog.hadPeers {
		t.Fatal("nil role did not reset discovery state")
	}
}
