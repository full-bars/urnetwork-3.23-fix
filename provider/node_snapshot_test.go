package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var snapT0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// --- rateSampler ---

func feed(s *rateSampler, start time.Time, samples ...map[string]uint64) time.Time {
	at := start
	for _, m := range samples {
		s.sample(m, at)
		at = at.Add(time.Second)
	}
	return at
}

func TestRateSamplerDeltas(t *testing.T) {
	s := newRateSampler()
	feed(s, snapT0,
		map[string]uint64{"a": 100, "b": 0}, // baseline, records nothing
		map[string]uint64{"a": 300, "b": 50},
		map[string]uint64{"a": 300, "b": 50},
		map[string]uint64{"a": 1300, "b": 150},
	)
	want := []int64{250, 0, 1100}
	if got := s.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	now, avg1m, avg5m := s.rates()
	if now != 1100 || avg1m != 450 || avg5m != 450 {
		t.Fatalf("rates = %d %d %d, want 1100 450 450", now, avg1m, avg5m)
	}
}

func TestRateSamplerEmpty(t *testing.T) {
	s := newRateSampler()
	if h := s.history(); h == nil || len(h) != 0 {
		t.Fatalf("history = %#v, want empty non-nil", h)
	}
	if a, b, c := s.rates(); a != 0 || b != 0 || c != 0 {
		t.Fatalf("rates = %d %d %d, want zeros", a, b, c)
	}
	// Baseline alone records nothing.
	s.sample(map[string]uint64{"a": 5}, snapT0)
	if len(s.history()) != 0 {
		t.Fatalf("baseline sample recorded a value")
	}
}

func TestRateSamplerIgnoresNewRemovedAndDecreasedKeys(t *testing.T) {
	s := newRateSampler()
	feed(s, snapT0,
		map[string]uint64{"a": 1000, "b": 1000},
		// b removed, c new with a large counter: neither counts.
		map[string]uint64{"a": 1100, "c": 9_000_000},
		// a respawned with a zeroed counter: decrease counts for nothing.
		map[string]uint64{"a": 10, "c": 9_000_500},
		// b comes back: it has no previous sample, so it is new again.
		map[string]uint64{"a": 60, "b": 7_000_000, "c": 9_000_500},
	)
	want := []int64{100, 500, 50}
	if got := s.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

func TestRateSamplerEmptyMapClearsBaseline(t *testing.T) {
	s := newRateSampler()
	feed(s, snapT0,
		map[string]uint64{"a": 1000},
		map[string]uint64{}, // pool went empty
		map[string]uint64{"a": 50_000_000},
	)
	// The return of "a" after an empty tick is a new key, not a huge delta.
	want := []int64{0, 0}
	if got := s.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

func TestRateSamplerDividesByElapsed(t *testing.T) {
	s := newRateSampler()
	s.sample(map[string]uint64{"a": 0}, snapT0)
	s.sample(map[string]uint64{"a": 4000}, snapT0.Add(4*time.Second))
	// Elapsed under one second is floored at one, like the billable writer.
	s.sample(map[string]uint64{"a": 4500}, snapT0.Add(4*time.Second+100*time.Millisecond))
	want := []int64{1000, 500}
	if got := s.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
}

func TestRateSamplerRingWrapAndAverages(t *testing.T) {
	s := newRateSampler()
	total := uint64(0)
	at := snapT0
	s.sample(map[string]uint64{"a": 0}, at)
	// 700 ticks, tick i adds i bytes, so sample i has value i (1..700).
	for i := 1; i <= 700; i++ {
		total += uint64(i)
		at = at.Add(time.Second)
		s.sample(map[string]uint64{"a": total}, at)
	}
	h := s.history()
	if len(h) != snapshotRingSize {
		t.Fatalf("len(history) = %d, want %d", len(h), snapshotRingSize)
	}
	if h[0] != 101 || h[len(h)-1] != 700 {
		t.Fatalf("history ends = %d..%d, want 101..700 oldest first", h[0], h[len(h)-1])
	}
	for i := 1; i < len(h); i++ {
		if h[i] != h[i-1]+1 {
			t.Fatalf("history not contiguous at %d: %d then %d", i, h[i-1], h[i])
		}
	}
	now, avg1m, avg5m := s.rates()
	// Last 60 are 641..700 (mean 670.5), last 300 are 401..700 (mean 550.5).
	if now != 700 || avg1m != 670 || avg5m != 550 {
		t.Fatalf("rates = %d %d %d, want 700 670 550", now, avg1m, avg5m)
	}
}

func TestRateSamplerAveragesOverAvailableSamples(t *testing.T) {
	s := newRateSampler()
	feed(s, snapT0,
		map[string]uint64{"a": 0},
		map[string]uint64{"a": 100},
		map[string]uint64{"a": 400},
	)
	_, avg1m, avg5m := s.rates()
	if avg1m != 200 || avg5m != 200 {
		t.Fatalf("averages = %d %d, want 200 200 (two samples)", avg1m, avg5m)
	}
}

// --- state derivation ---

func TestDeriveSnapshotState(t *testing.T) {
	healthy := SnapshotProxies{Up: 10}
	cases := []struct {
		name string
		in   stateInputs
		want string
	}{
		{"starting wins over everything", stateInputs{uptime: 119 * time.Second, proxies: SnapshotProxies{Dead: 10}, pressure: 1, avg1m: 1e9}, "starting"},
		{"starting ends at 120s", stateInputs{uptime: 120 * time.Second, proxies: healthy, avg1m: 1e9}, "flowing"},
		{"degraded: more than half dead", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 4, Dead: 6}, avg1m: 1e9}, "degraded"},
		{"degraded: dead plus degraded", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 4, Dead: 3, Degraded: 3}, avg1m: 1e9}, "degraded"},
		{"exactly half is not degraded", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 5, Dead: 5}, avg1m: 1e9}, "flowing"},
		{"connecting does not count as bad", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 1, Connecting: 9}, avg1m: 1e9}, "flowing"},
		{"no proxies is not degraded", stateInputs{uptime: time.Hour, avg1m: 0}, "idle"},
		{"pressure at 0.8", stateInputs{uptime: time.Hour, proxies: healthy, pressure: 0.8, avg1m: 1e9}, "degraded"},
		{"pressure just under 0.8", stateInputs{uptime: time.Hour, proxies: healthy, pressure: 0.79, avg1m: 1e9}, "flowing"},
		{"degraded beats idle", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Dead: 10}, avg1m: 0}, "degraded"},
		{"idle below threshold", stateInputs{uptime: time.Hour, proxies: healthy, avg1m: 5119}, "idle"},
		{"flowing at threshold", stateInputs{uptime: time.Hour, proxies: healthy, avg1m: 5120}, "flowing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSnapshotState(tc.in); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func hist(samples ...cumulativeSample) []cumulativeSample {
	for i := range samples {
		samples[i].at = snapT0.Add(time.Duration(i) * snapshotHistoryInterval)
	}
	return samples
}

// histAt builds a history whose samples are spaced one snapshotHistoryInterval
// apart and END at now, so "the last minute" windows line up with the test.
func histAt(now time.Time, samples ...cumulativeSample) []cumulativeSample {
	for i := range samples {
		samples[i].at = now.Add(-time.Duration(len(samples)-1-i) * snapshotHistoryInterval)
	}
	return samples
}

// A trickle of failed auths is normal steady state on a big paid pool (a few
// percent of the pool retrying every minute). Only a real wave, or a pool that
// mostly cannot connect, is worth blaming on auth.
func TestDeriveIdleHint(t *testing.T) {
	now := snapT0.Add(30 * time.Minute)
	healthy := SnapshotProxies{Up: 90, Connecting: 8, Degraded: 2} // 90% up

	// n failures spread over the last minute (6 intervals), no contract growth.
	failing := func(perInterval int64, contracts int64) []cumulativeSample {
		var ss []cumulativeSample
		for i := int64(0); i <= 6; i++ {
			ss = append(ss, cumulativeSample{auth: 1000 + i*perInterval, contracts: contracts})
		}
		return histAt(now, ss...)
	}
	steady := failing(1, 10) // 6 failures in the last minute on a 100 pool: 6%
	wave := failing(5, 10)   // 30 in the last minute on a 100 pool: 30%
	quiet := failing(0, 10)
	contractsUp := histAt(now, cumulativeSample{auth: 1000, contracts: 10}, cumulativeSample{auth: 1000, contracts: 11})

	cases := []struct {
		name    string
		proxies SnapshotProxies
		hist    []cumulativeSample
		want    string
	}{
		{"no proxies wins", SnapshotProxies{}, wave, "no proxies configured"},
		{"all dead or connecting", SnapshotProxies{Connecting: 12, Dead: 46}, wave, "all 58 proxies dead or connecting"},
		{"lone connection connecting", SnapshotProxies{Connecting: 1}, wave, "the only connection is dead or connecting"},
		{"lone connection dead", SnapshotProxies{Dead: 1}, wave, "the only connection is dead or connecting"},

		{"a steady retry trickle is not an auth problem", healthy, steady,
			"no contracts acquired in the last 10 min (90/100 proxies up, ~6 auth retries/min)"},
		{"trickle but contracts flowing is just no traffic", healthy, histAt(now,
			cumulativeSample{auth: 1000, contracts: 10}, cumulativeSample{auth: 1001, contracts: 11}),
			"no traffic offered (90/100 proxies up, ~1 auth retries/min)"},
		{"a failure wave is auth failing", healthy, wave, "auth failing: 30 failures in the last minute across 100 proxies"},
		{"majority unconnected with failures is auth failing", SnapshotProxies{Up: 30, Connecting: 70}, steady,
			"auth failing: only 30 of 100 proxies authenticated"},
		{"majority unconnected without failures is not blamed on auth", SnapshotProxies{Up: 30, Connecting: 70}, quiet,
			"only 30 of 100 proxies connected"},
		{"exactly half up is not a majority down", SnapshotProxies{Up: 50, Connecting: 50}, quiet,
			"no contracts acquired in the last 10 min (50/100 proxies up)"},

		{"no history counts as no contracts", healthy, nil, "no contracts acquired in the last 10 min (90/100 proxies up)"},
		{"contracts flowing in, no failures", healthy, contractsUp, "no traffic offered (90/100 proxies up)"},
		{"an auth counter decrease is not failing", healthy,
			histAt(now, cumulativeSample{auth: 9000, contracts: 10}, cumulativeSample{auth: 3, contracts: 10}, cumulativeSample{auth: 3, contracts: 10}),
			"no contracts acquired in the last 10 min (90/100 proxies up)"},
		{"old failures outside the last minute are not a wave", healthy,
			func() []cumulativeSample {
				h := histAt(now,
					cumulativeSample{auth: 0, contracts: 10}, cumulativeSample{auth: 500, contracts: 10},
					cumulativeSample{auth: 500, contracts: 10}, cumulativeSample{auth: 500, contracts: 10},
					cumulativeSample{auth: 500, contracts: 10}, cumulativeSample{auth: 500, contracts: 10},
					cumulativeSample{auth: 500, contracts: 10}, cumulativeSample{auth: 500, contracts: 10},
					cumulativeSample{auth: 500, contracts: 10})
				return h
			}(), "no contracts acquired in the last 10 min (90/100 proxies up)"},
		{"a small pool needs a floor before a wave counts", SnapshotProxies{Up: 3, Connecting: 1}, histAt(now,
			cumulativeSample{auth: 0, contracts: 10}, cumulativeSample{auth: 1, contracts: 10}),
			"no contracts acquired in the last 10 min (3/4 proxies up, ~1 auth retries/min)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveIdleHint(tc.proxies, tc.hist, now); got != tc.want {
				t.Fatalf("hint = %q, want %q", got, tc.want)
			}
		})
	}
}

// Chicago's real steady state (measured): ~229 failed auths a minute on a
// ~4,800 proxy pool with ~430 still connecting. That must not read as an
// outage, and the hint must say what is actually true.
func TestDeriveIdleHintBigPoolSteadyStateIsNotAuthFailing(t *testing.T) {
	now := snapT0.Add(30 * time.Minute)
	var ss []cumulativeSample
	for i := int64(0); i <= 6; i++ {
		ss = append(ss, cumulativeSample{auth: 10000 + i*38, contracts: 500 + i}) // ~229/min, contracts still arriving
	}
	got := deriveIdleHint(SnapshotProxies{Up: 4056, Degraded: 30, Connecting: 427}, histAt(now, ss...), now)
	want := "no traffic offered (4056/4513 proxies up, ~228 auth retries/min)"
	if got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}
}

func TestCumulativeHistoryCadenceAndCap(t *testing.T) {
	var h cumulativeHistory
	at := snapT0
	if !h.due(at) {
		t.Fatal("empty history should be due")
	}
	h.add(cumulativeSample{at: at, auth: 1})
	if h.due(at.Add(9 * time.Second)) {
		t.Fatal("due before interval elapsed")
	}
	if !h.due(at.Add(10 * time.Second)) {
		t.Fatal("not due after interval elapsed")
	}
	for i := 2; i <= snapshotHistorySize+20; i++ {
		at = at.Add(snapshotHistoryInterval)
		h.add(cumulativeSample{at: at, auth: int64(i)})
	}
	got := h.copy()
	if len(got) != snapshotHistorySize {
		t.Fatalf("len = %d, want %d", len(got), snapshotHistorySize)
	}
	if got[len(got)-1].auth != int64(snapshotHistorySize+20) || got[0].auth != 21 {
		t.Fatalf("ring kept the wrong window: first %d last %d", got[0].auth, got[len(got)-1].auth)
	}
}

// --- restart pending ---

func TestRestartPendingFor(t *testing.T) {
	startup := map[string]string{"ramlogs": "1", "profile": "turbo-v4"}
	cur := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		name string
		cur  map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"same values", map[string]string{"ramlogs": "1", "profile": "turbo-v4"}, false},
		{"ramlogs spelled differently", map[string]string{"ramlogs": "on"}, false},
		{"ramlogs turned off", map[string]string{"ramlogs": "off"}, true},
		{"profile changed", map[string]string{"profile": "eco"}, true},
		{"other keys ignored", map[string]string{"gogc": "50", "node_name": "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartPendingFor(cur(tc.cur), startup); got != tc.want {
				t.Fatalf("pending = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- collector ---

type fakeSnapshotEnv struct {
	now         time.Time
	billable    map[string]uint64
	proxies     SnapshotProxies
	clients     int64
	auth        int64
	contracts   int64
	pressure    float64
	pending     bool
	startup     string
	traffic     map[string]uint64
	lifetime    uint64
	hasLifetime bool
	reads       int
}

func (f *fakeSnapshotEnv) sources() snapshotSources {
	return snapshotSources{
		now:       func() time.Time { return f.now },
		startedAt: snapT0,
		billable:  func() map[string]uint64 { return f.billable },
		pool: func() (SnapshotProxies, int64) {
			f.reads++
			return f.proxies, f.clients
		},
		cumulative:       func() (int64, int64) { return f.auth, f.contracts },
		sessions:         func() (int64, int64) { return 7, 3 },
		pressure:         func() float64 { return f.pressure },
		version:          func() string { return "v9" },
		prevVer:          func() string { return "v8" },
		restart:          func() SnapshotRestart { return SnapshotRestart{Reason: "update", CleanShutdown: true} },
		resources:        func() SnapshotResources { return SnapshotResources{HeapInuseBytes: 5, Goroutines: 2} },
		restartPending:   func() bool { return f.pending },
		startup:          func() string { return f.startup },
		traffic:          func() map[string]uint64 { return f.traffic },
		lifetimeBillable: func() (uint64, bool) { return f.lifetime, f.hasLifetime },
	}
}

func TestCollectorBuildsFlowingSnapshot(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 3}, clients: 4, billable: map[string]uint64{"a": 0}}
	c := newNodeSnapshotCollector(env.sources())
	for i := 0; i < 200; i++ {
		env.now = env.now.Add(time.Second)
		env.billable = map[string]uint64{"a": uint64(i) * 1_000_000}
		c.tick()
	}
	env.pending = true
	snap := c.Get()

	if snap.V != 1 || snap.Version != "v9" || snap.PreviousVersion != "v8" {
		t.Errorf("identity wrong: %+v", snap)
	}
	if snap.State != "flowing" || !snap.Busy || snap.IdleHint != "" {
		t.Errorf("state = %q busy=%v hint=%q", snap.State, snap.Busy, snap.IdleHint)
	}
	if !snap.RestartPending {
		t.Error("restart_pending not set")
	}
	if snap.Rate.NowBps != 1_000_000 || snap.Rate.Avg1mBps != 1_000_000 || snap.Rate.HistoryInterval != 1 {
		t.Errorf("rate = %+v", snap.Rate)
	}
	if snap.Clients != 4 || snap.Sessions.PQE != 7 || snap.Sessions.Classical != 3 || snap.Proxies.Up != 3 {
		t.Errorf("counts wrong: %+v", snap)
	}
	if snap.UptimeSeconds != 200 || !snap.StartedAt.Equal(snapT0) {
		t.Errorf("uptime %v started %v", snap.UptimeSeconds, snap.StartedAt)
	}
}

func TestCollectorIdleHintOnlyWhenIdle(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0, billable: map[string]uint64{}}
	c := newNodeSnapshotCollector(env.sources())
	for i := 0; i < 30; i++ {
		env.now = env.now.Add(10 * time.Second)
		c.tick()
	}
	snap := c.Get()
	if snap.State != "idle" || snap.Busy {
		t.Fatalf("state = %q busy=%v, want idle", snap.State, snap.Busy)
	}
	if snap.IdleHint != "no proxies configured" {
		t.Fatalf("hint = %q", snap.IdleHint)
	}
	if snap.Rate.HistoryBps == nil {
		t.Fatal("history must marshal as [], not null")
	}
}

func TestCollectorAuthFailingHintUsesHistory(t *testing.T) {
	// A wave: the failures land inside the last minute, so auth is blamed.
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 2}, billable: map[string]uint64{}}
	c := newNodeSnapshotCollector(env.sources())
	c.tick()
	env.now = env.now.Add(150 * time.Second)
	c.tick()
	env.now = env.now.Add(150 * time.Second) // 300s uptime, past starting
	env.auth = 4
	c.tick()
	snap := c.Get()
	if snap.State != "idle" || snap.IdleHint != "auth failing: 4 failures in the last minute across 2 proxies" {
		t.Fatalf("state=%q hint=%q", snap.State, snap.IdleHint)
	}
}

func TestCollectorOldAuthFailuresDoNotBlameAuth(t *testing.T) {
	// The same failures 150s ago are history, not a wave: the hint falls through
	// to what is actually true instead of reporting a stale outage.
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 2}, billable: map[string]uint64{}}
	c := newNodeSnapshotCollector(env.sources())
	c.tick()
	env.now = env.now.Add(150 * time.Second)
	env.auth = 4
	c.tick()
	env.now = env.now.Add(150 * time.Second)
	c.tick()
	snap := c.Get()
	if snap.State != "idle" || snap.IdleHint != "no contracts acquired in the last 10 min (2/2 proxies up)" {
		t.Fatalf("state=%q hint=%q", snap.State, snap.IdleHint)
	}
}

func TestCollectorStartingState(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0.Add(30 * time.Second), proxies: SnapshotProxies{Up: 1}}
	c := newNodeSnapshotCollector(env.sources())
	if snap := c.Get(); snap.State != "starting" || snap.IdleHint != "" {
		t.Fatalf("state=%q hint=%q, want starting with no hint", snap.State, snap.IdleHint)
	}
}

func TestCollectorGetCachesForOneSecond(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0.Add(time.Hour), proxies: SnapshotProxies{Up: 1}}
	c := newNodeSnapshotCollector(env.sources())

	first := c.Get()
	env.now = env.now.Add(999 * time.Millisecond)
	if second := c.Get(); second != first || env.reads != 1 {
		t.Fatalf("Get inside the cache window rebuilt (reads=%d)", env.reads)
	}
	env.now = env.now.Add(time.Millisecond)
	if third := c.Get(); third == first || env.reads != 2 {
		t.Fatalf("Get after the cache window did not rebuild (reads=%d)", env.reads)
	}
}

// --- wire contract ---

func TestNodeSnapshotMatchesFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "livestatus", "node_snapshot_v1*.json"))
	if err != nil || len(files) < 2 {
		t.Fatalf("fixtures not found: %v (err %v)", files, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var snap NodeSnapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			out, err := json.Marshal(snap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip differs from fixture\nfixture: %s\nremarshal: %s", raw, out)
			}
		})
	}
}

func TestNodeSnapshotIgnoresUnknownFields(t *testing.T) {
	in := `{"v":2,"state":"flowing","future_field":{"a":1},"rate":{"now_bps":1,"history_bps":[],"extra":true}}`
	var snap NodeSnapshot
	if err := json.Unmarshal([]byte(in), &snap); err != nil {
		t.Fatalf("unknown fields must be tolerated: %v", err)
	}
	if snap.V != 2 || snap.Rate.NowBps != 1 {
		t.Fatalf("known fields lost: %+v", snap)
	}
}

func TestNodeSnapshotOptionalFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(NodeSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"previous_version", "idle_hint", "mem_limit_bytes", "rss_bytes", "open_fds", "fd_limit"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s present in zero snapshot: %s", k, b)
		}
	}
}
