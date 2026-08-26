package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// proxySet indexes a candidate slice by address for set-comparison.
func proxySet(items []removedProxy) map[string]bool {
	m := map[string]bool{}
	for _, rp := range items {
		m[rp.addr] = true
	}
	return m
}

// assertCandidatesEqual asserts the collected candidate list contains exactly
// the want addresses (order-independent).
func assertCandidatesEqual(t *testing.T, got []removedProxy, want ...string) {
	t.Helper()
	g := proxySet(got)
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for s := range w {
		if !g[s] {
			t.Errorf("missing candidate %q (got %v)", s, want)
		}
	}
	for s := range g {
		if !w[s] {
			t.Errorf("unexpected candidate %q", s)
		}
	}
}

// collectTests exercises the pure decision core of `proxy remove-dead`
// (collectRemoveDeadCandidates). Because it is a pure function the tests feed
// it a state with explicit Health verdicts (the output of the real health
// prober) and a backdated uptime — simulating a settled, ~70-min-old provider
// with no wait. This directly covers the removal-selection logic that the
// uptime gate otherwise hides from functional CI.
func TestCollectRemoveDeadCandidates(t *testing.T) {
	now := time.Now()
	downOld := now.Add(-48 * time.Hour).Format(time.RFC3339)
	downRecent := now.Add(-12 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name     string
		stateSrc string
		proxies  map[string]ProxyEntry
		opts     removeDeadOptions
		uptime   time.Duration
		wantDead []string
		wantIna  []string
		wantDeg  []string
		wantAuth []string
	}{
		{
			name: "dead+inactive+up",
			proxies: map[string]ProxyEntry{
				"d": {ID: 1, Health: "dead", Source: "file"},
				"i": {ID: 2, Health: "inactive", Source: "file"},
				"u": {ID: 3, Health: "up", Source: "file"},
			},
			wantDead: []string{"d"},
			wantIna:  []string{"i"},
		},
		{
			name:     "source-filter picks url only; untagged resolves via state.Source",
			stateSrc: "proxy.txt",
			proxies: map[string]ProxyEntry{
				"f1": {ID: 1, Health: "dead", Source: "file"},
				"u1": {ID: 2, Health: "dead", Source: "url"},
				"un": {ID: 3, Health: "dead"}, // untagged -> resolves to "file" (state.Source set)
			},
			opts:     removeDeadOptions{sourceFilter: "url"},
			wantDead: []string{"u1"},
		},
		{
			name:     "untagged resolves to internal when no state.Source",
			stateSrc: "",
			proxies: map[string]ProxyEntry{
				"un": {ID: 1, Health: "dead"},
			},
			opts:     removeDeadOptions{sourceFilter: "internal"},
			wantDead: []string{"un"},
		},
		{
			name: "degraded honors down-since window; recent kept",
			proxies: map[string]ProxyEntry{
				"old": {ID: 1, Health: "offline", Source: "file", DownSince: downOld},
				"new": {ID: 2, Health: "offline", Source: "file", DownSince: downRecent},
			},
			opts:    removeDeadOptions{degradedDur: 24 * time.Hour},
			uptime:  2 * time.Hour,
			wantDeg: []string{"old"},
		},
		{
			name: "degraded: without --degraded (degradedDur=0) offline proxies are NOT degraded",
			proxies: map[string]ProxyEntry{
				"old": {ID: 1, Health: "offline", Source: "file", DownSince: downOld},
			},
			opts:   removeDeadOptions{degradedDur: 0},
			uptime: 2 * time.Hour,
			// Old down-since but NO --degraded -> must NOT be collected (matches auto path).
			wantDeg: []string{},
		},
		{
			name: "unparsable down-since is fail-closed (not removed)",
			proxies: map[string]ProxyEntry{
				"bad": {ID: 1, Health: "offline", Source: "file", DownSince: "not-a-time"},
			},
			opts:    removeDeadOptions{degradedDur: 24 * time.Hour},
			uptime:  2 * time.Hour,
			wantDeg: []string{},
		},
		{
			name: "auth-failing boundary + up-guard + dead double-count",
			proxies: map[string]ProxyEntry{
				"up":      {ID: 1, Health: "up", Source: "file", AuthFailures: 1000},
				"off_hi":  {ID: 2, Health: "offline", Source: "file", AuthFailures: 200},
				"off_lo":  {ID: 3, Health: "offline", Source: "file", AuthFailures: 10},
				"dead_hi": {ID: 4, Health: "dead", Source: "file", AuthFailures: 500},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   48 * time.Hour, // days = 2 -> threshold = 100*2 = 200
			wantDead: []string{"dead_hi"},
			// degradedDur=0 (no --degraded) => offline entries are NOT degraded.
			wantDeg:  []string{},
			wantAuth: []string{"off_hi", "dead_hi"}, // dead_hi double-counts (dead AND auth-failing) - pinned
		},
		{
			name: "auth days-multiplier truncates at 47h59m (days=1) not 48h (days=2)",
			proxies: map[string]ProxyEntry{
				"e": {ID: 1, Health: "offline", Source: "file", AuthFailures: 150},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   47*time.Hour + 59*time.Minute, // days = 1 -> threshold 100 -> 150 >= 100 selected
			wantDeg:  []string{},
			wantAuth: []string{"e"},
		},
		{
			name: "auth days-multiplier at 48h (days=2) raises threshold above 150",
			proxies: map[string]ProxyEntry{
				"e": {ID: 1, Health: "offline", Source: "file", AuthFailures: 150},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   48 * time.Hour, // days = 2 -> threshold 200 -> 150 < 200 NOT selected
			wantDeg:  []string{},
			wantAuth: []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := &ProxyState{Source: c.stateSrc, StartedAt: time.Now().Add(-2 * time.Hour), Proxies: c.proxies}
			if c.uptime == 0 {
				c.uptime = 2 * time.Hour
			}
			dead, inactive, degraded, authFailing := collectRemoveDeadCandidates(state, c.opts, c.uptime)
			assertCandidatesEqual(t, dead, c.wantDead...)
			assertCandidatesEqual(t, inactive, c.wantIna...)
			assertCandidatesEqual(t, degraded, c.wantDeg...)
			assertCandidatesEqual(t, authFailing, c.wantAuth...)
		})
	}
}

// TestRemoveDeadCommandChainsToSources proves the seam between the selection
// logic and the actual removal: proxies selected by collectRemoveDeadCandidates
// are bucketed into the right source and physically removed from the real proxy
// file, while live ones survive. This is the end-to-end "command prunes"
// guarantee without waiting ~an hour for a provider to reach the uptime gate.
func TestRemoveDeadCommandChainsToSources(t *testing.T) {
	home := withTempHome(t)
	proxyFilePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(proxyFilePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{
		Source:    proxyFilePath,
		StartedAt: time.Now().Add(-80 * time.Minute), // a settled provider past the 65-min gate
		Proxies: map[string]ProxyEntry{
			"1.1.1.1:1080": {Health: "dead", Source: "file"},
			"2.2.2.2:1080": {Health: "up", Source: "file"},
		},
	}

	dead, _, _, _ := collectRemoveDeadCandidates(state, removeDeadOptions{}, 80*time.Minute)
	assertCandidatesEqual(t, dead, "1.1.1.1:1080")

	// Same bucketing logic the command uses (proxyRemoveDead).
	addrsBySource := map[string][]string{}
	for _, rp := range dead {
		src := rp.entry.Source
		if src == "" {
			src = "file"
		}
		addrsBySource[src] = append(addrsBySource[src], rp.addr)
	}
	if err := removeDeadProxies(state, addrsBySource); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(proxyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "2.2.2.2:1080:u:p\n" {
		t.Fatalf("live proxy was not preserved / dead proxy not removed; got %q", got)
	}
}
