package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The hover detail names the cached UDP check state of each proxy, using the
// same rules as the coverage counts: a missing, running or expired result is
// unchecked, and the direct slot has no proxy to check.
func TestProxyCellDetailUDPStatus(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	saved := globalProxyUDPCheck
	defer func() { globalProxyUDPCheck = saved }()
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"ok":       {result: proxyUDPResult{Status: proxyUDPOK}, at: now},
		"no":       {result: proxyUDPResult{Status: proxyUDPNoRelay}, at: now},
		"mismatch": {result: proxyUDPResult{Status: proxyUDPExitMismatch}, at: now},
		"unknown":  {result: proxyUDPResult{Status: proxyUDPUnknown}, at: now},
		"expired":  {result: proxyUDPResult{Status: proxyUDPOK}, at: now.Add(-proxyUDPOKTTL)},
		"running":  {done: make(chan struct{})},
	}
	globalProxyUDPCheck = store
	for _, c := range []struct {
		index int
		key   string
		want  string
	}{
		{1, "ok", "ok"},
		{2, "no", "no_relay"},
		{3, "mismatch", "mismatch"},
		{4, "unknown", "unknown"},
		{5, "expired", "unchecked"},
		{6, "running", "unchecked"},
		{7, "missing", "unchecked"},
		{0, "direct", "direct"},
	} {
		if got := proxyCellUDPStatus(connect.ProxyCellRaw{Index: c.index, Key: c.key}, now); got != c.want {
			t.Errorf("proxy %d (%s): udp %q, want %q", c.index, c.key, got, c.want)
		}
	}
}
