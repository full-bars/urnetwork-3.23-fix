package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestCapProxyList(t *testing.T) {
	if got := capProxyList(nil, 50); got != "" {
		t.Fatalf("empty = %q, want empty", got)
	}
	if got := capProxyList([]string{"a", "b"}, 50); got != "a, b" {
		t.Fatalf("under cap = %q, want \"a, b\"", got)
	}
	got := capProxyList([]string{"a", "b", "c"}, 2)
	if got != "a, b, ... (+1 more)" {
		t.Fatalf("over cap = %q, want \"a, b, ... (+1 more)\"", got)
	}
}

func TestFormatStateFile(t *testing.T) {
	r := connect.ProxyHealthReport{
		Up:       3,
		Dead:     []string{"proxy[2] (c:1)"},
		Degraded: []string{"proxy[1] (b:1)"},
		LifetimeRecovered: 5,
		LifetimeLost:      4,
	}
	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	out := formatStateFile(r, now)

	if !strings.Contains(out, "up=3 down=2 dead=1 degraded=1") {
		t.Fatalf("missing summary header in:\n%s", out)
	}
	if !strings.Contains(out, "lifetime_recovered=5 lifetime_lost=4") {
		t.Fatalf("missing lifetime header in:\n%s", out)
	}
	if !strings.Contains(out, "DEAD     proxy[2] (c:1)") {
		t.Fatalf("missing dead line in:\n%s", out)
	}
	if !strings.Contains(out, "DEGRADED proxy[1] (b:1)") {
		t.Fatalf("missing degraded line in:\n%s", out)
	}
}

func TestFormatEventLines(t *testing.T) {
	r := connect.ProxyHealthReport{
		Recovered:     []connect.ProxyEvent{{Index: 1, Address: "b:1", After: 55*time.Minute + 8*time.Second}},
		NewlyDegraded: []connect.ProxyEvent{{Index: 3, Address: "d:1"}},
		NewlyDead:     []connect.ProxyEvent{{Index: 2, Address: "c:1"}},
	}
	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	lines := formatEventLines(r, now)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "2026-06-02T16:05:11Z RECOVERED proxy[1] (b:1) after=55m8s") {
		t.Fatalf("missing/!= recovered line in:\n%s", joined)
	}
	if !strings.Contains(joined, "2026-06-02T16:05:11Z DEGRADED  proxy[3] (d:1)") {
		t.Fatalf("missing degraded line in:\n%s", joined)
	}
	if !strings.Contains(joined, "2026-06-02T16:05:11Z DEAD      proxy[2] (c:1)") {
		t.Fatalf("missing dead line in:\n%s", joined)
	}
}

func TestFormatEventLinesRecoveredWithoutLatency(t *testing.T) {
	r := connect.ProxyHealthReport{
		Recovered: []connect.ProxyEvent{{Index: 0, Address: "a:1"}}, // After == 0 -> omit
	}
	now := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)
	lines := formatEventLines(r, now)
	if len(lines) != 1 || strings.Contains(lines[0], "after=") {
		t.Fatalf("recovered without latency should omit after=, got %v", lines)
	}
}
