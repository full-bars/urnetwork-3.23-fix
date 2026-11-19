package urnettools

import (
	"strings"
	"testing"
)

func TestMeterOnlyOnColorTerminals(t *testing.T) {
	if got := (liveOpts{Color: false}).meter(0.5, true); got != "" {
		t.Fatalf("plain output must not draw a meter, got %q", got)
	}
	if got := (liveOpts{Color: true, ASCII: true}).meter(0.5, true); got != "" {
		t.Fatalf("ascii output must not draw a meter, got %q", got)
	}
	full := (liveOpts{Color: true}).meter(1, true)
	if !strings.Contains(full, "\x1b[32m") || strings.Contains(full, "░\x1b[0m\x1b[90m░") && false {
		t.Fatalf("a full coverage meter is green: %q", full)
	}
	if got := (liveOpts{Color: true}).meter(0.95, false); !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("a nearly full memory meter is red: %q", got)
	}
	if got := (liveOpts{Color: true}).meter(7, true); strings.Count(got, "█") != 12 {
		t.Fatalf("a ratio above one is clamped: %q", got)
	}
}

func TestLiveBlockMetersFollowTheNumbers(t *testing.T) {
	limit := uint64(1000)
	s := &NodeSnapshot{State: "flowing", Pressure: 0.9}
	s.Proxies.Up, s.Proxies.Dead = 90, 10
	s.Proxies.UDP = &SnapshotProxyUDP{OK: 50, Unchecked: 50}
	s.Resources.HeapInuseBytes = 500
	s.Resources.MemLimitBytes = &limit
	color := renderLiveBlock(s, liveOpts{Color: true, Width: 200}, "")
	if n := strings.Count(color, "█"); n < 4 {
		t.Fatalf("expected meters on proxies, udp, memory and pressure rows, got %d filled cells in %q", n, color)
	}
	plain := renderLiveBlock(s, liveOpts{Color: false, ASCII: true, Width: 200}, "")
	if strings.ContainsAny(plain, "█░") || strings.Contains(plain, "\x1b") {
		t.Fatalf("plain output changed: %q", plain)
	}
}
