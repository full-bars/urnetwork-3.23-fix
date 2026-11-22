package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyCellsIPv6For(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"valid-yes":   {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::1", At: now, Interval: time.Hour}},
		"valid-udp":   {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, UDPExit: "2001:db8::2", At: now, Interval: time.Hour}},
		"valid-no":    {ipv6: proxyIPv6Result{Status: proxyIPv6No, At: now, Interval: time.Hour}},
		"valid-unk":   {ipv6: proxyIPv6Result{Status: proxyIPv6Unknown, At: now, Interval: time.Hour}},
		"invalid-ips": {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "not-an-ip", At: now, Interval: time.Hour}},
		"ipv4-in-v6":  {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "192.0.2.1", At: now, Interval: time.Hour}},
		"expired":     {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::3", At: now.Add(-3 * time.Hour), Interval: time.Hour}},
	}

	for _, c := range []struct {
		key      string
		wantStat string
		wantExit string
	}{
		{"valid-yes", "yes", "2001:db8::1"},
		{"valid-udp", "yes", "2001:db8::2"},
		{"valid-no", "no", ""},
		{"valid-unk", "unknown", ""},
		{"invalid-ips", "yes", ""},
		{"ipv4-in-v6", "yes", ""},
		{"expired", "unknown", ""},
		{"missing", "unknown", ""},
	} {
		stat, exit := store.ipv6For(c.key, now)
		if stat != c.wantStat || exit != c.wantExit {
			t.Errorf("ipv6For(%q): got (%q, %q), want (%q, %q)", c.key, stat, exit, c.wantStat, c.wantExit)
		}
	}
}

func TestProxyCellsDetailIPv6(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"p1": {ipv6: proxyIPv6Result{Status: proxyIPv6Yes, TCPExit: "2001:db8::1", At: now, Interval: time.Hour}},
	}
	saved := globalProxyUDPCheck
	defer func() { globalProxyUDPCheck = saved }()
	globalProxyUDPCheck = store

	cellStore := &proxyCellStore{
		raw: []connect.ProxyCellRaw{
			{Index: 0, Key: "direct", Addr: "127.0.0.1:0"},
			{Index: 1, Key: "p1", Addr: "192.0.2.1:1080"},
		},
		at:     now,
		deltas: map[int]uint64{},
	}
	details := cellStore.Detail(now)
	if len(details) != 2 {
		t.Fatalf("expected 2 details, got %d", len(details))
	}
	// Direct slot (Index 0) must NOT have IPv6 or IPv6Exit
	if details[0].IPv6 != "" || details[0].IPv6Exit != "" {
		t.Fatalf("direct slot should not have IPv6 details: got %q %q", details[0].IPv6, details[0].IPv6Exit)
	}
	// Proxy slot (Index 1) must have IPv6 and IPv6Exit
	if details[1].IPv6 != "yes" || details[1].IPv6Exit != "2001:db8::1" {
		t.Fatalf("p1 slot got IPv6 %q %q, want yes 2001:db8::1", details[1].IPv6, details[1].IPv6Exit)
	}
}
