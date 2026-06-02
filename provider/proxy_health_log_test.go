package main

import (
	"os"
	"path/filepath"
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

	if !strings.Contains(out, "Up: 3 | Down: 2 | Dead: 1 | Degraded: 1") {
		t.Fatalf("missing summary header in:\n%s", out)
	}
	if !strings.Contains(out, "Lifetime Recovered: 5 | Lifetime Lost: 4") {
		t.Fatalf("missing lifetime header in:\n%s", out)
	}
	if !strings.Contains(out, "| DEAD     | proxy[2]    | c:1                             |") {
		t.Fatalf("missing dead line in:\n%s", out)
	}
	if !strings.Contains(out, "| DEGRADED | proxy[1]    | b:1                             |") {
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
	if !strings.Contains(joined, "| 2026-06-02T16:05:11Z | RECOVERED | proxy[1]    | b:1                   | after=55m8s   |") {
		t.Fatalf("missing/!= recovered line in:\n%s", joined)
	}
	if !strings.Contains(joined, "| 2026-06-02T16:05:11Z | DEGRADED  | proxy[3]    | d:1                   |               |") {
		t.Fatalf("missing degraded line in:\n%s", joined)
	}
	if !strings.Contains(joined, "| 2026-06-02T16:05:11Z | DEAD      | proxy[2]    | c:1                   |               |") {
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

func TestRotateIfNeeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_health.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0644); err != nil {
		t.Fatal(err)
	}

	// Under the cap: no rotation.
	rotateIfNeeded(path, 1000)
	if _, err := os.Stat(filepath.Join(dir, "proxy_health.log.1")); !os.IsNotExist(err) {
		t.Fatalf("rotated under cap, want no .1 file")
	}

	// Over the cap: rotate to .1, original gone.
	rotateIfNeeded(path, 50)
	if _, err := os.Stat(filepath.Join(dir, "proxy_health.log.1")); err != nil {
		t.Fatalf(".1 file missing after rotation: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original log still present after rotation")
	}
}

func TestWriteProxyHealthFiles(t *testing.T) {
	dir := t.TempDir()
	r := connect.ProxyHealthReport{
		Up:        1,
		Dead:      []string{"proxy[2] (c:1)"},
		NewlyDead: []connect.ProxyEvent{{Index: 2, Address: "c:1"}},
	}
	now := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)

	writeProxyHealthState(dir, r, now)
	writeProxyHealthEvents(dir, r, now)

	state, err := os.ReadFile(filepath.Join(dir, "proxy_health.state"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "| DEAD     | proxy[2]    | c:1                             |") {
		t.Fatalf("state file missing dead entry:\n%s", state)
	}

	events, err := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "| DEAD      | proxy[2]    | c:1                   |               |") {
		t.Fatalf("event log missing dead entry:\n%s", events)
	}

	// No events -> event log unchanged (no empty append).
	before, _ := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	writeProxyHealthEvents(dir, connect.ProxyHealthReport{}, now)
	after, _ := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	if string(before) != string(after) {
		t.Fatalf("empty report should not append to event log")
	}
}
