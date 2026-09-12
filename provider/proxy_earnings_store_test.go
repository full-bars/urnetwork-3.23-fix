package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// bwWith builds a bandwidth snapshot entry carrying a cumulative billable
// total split across rx and tx, the shape the health snapshot produces.
func bwWith(billable uint64) *connect.ProxyBandwidth {
	bw := &connect.ProxyBandwidth{}
	bw.BillableRx.Store(billable)
	return bw
}

func TestEarningsScoreUnknownAddressIsZero(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	if got := s.Score("1.2.3.4:1080", time.Now()); got != 0 {
		t.Fatalf("unknown address score = %v, want 0", got)
	}
}

func TestEarningsFirstObservationOnlySetsBaseline(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	// A proxy first seen already carrying a cumulative total must not be
	// credited with that total: the counter is cumulative for the process,
	// not earnings observed by this store.
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(5000)}, now)

	if got := s.Score("a:1080", now); got != 0 {
		t.Fatalf("score after baseline observation = %v, want 0", got)
	}
}

func TestEarningsPositiveDeltaRaisesScore(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(1000)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(3500)}, now)

	if got := s.Score("a:1080", now); got != 2500 {
		t.Fatalf("score after 2500-byte delta = %v, want 2500", got)
	}
}

func TestEarningsCounterResetContributesNothing(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(1000)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(4000)}, now)
	// The proxy restarts and its counter resets. A backwards counter is not
	// negative earnings; it re-baselines.
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(10)}, now)

	if got := s.Score("a:1080", now); got != 3000 {
		t.Fatalf("score after counter reset = %v, want 3000 (unchanged)", got)
	}
	// And the re-baseline must hold: the next delta is measured from 10.
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(110)}, now)
	if got := s.Score("a:1080", now); got != 3100 {
		t.Fatalf("score after post-reset delta = %v, want 3100", got)
	}
}

func TestEarningsScoreHalvesOverOneHalfLife(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(4000)}, now)

	got := s.Score("a:1080", now.Add(earningsHalfLife))
	if got < 1990 || got > 2010 {
		t.Fatalf("score after one half-life = %v, want ~2000", got)
	}
}

func TestEarningsHistorySurvivesProxyLeavingTheSnapshot(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(9000)}, now)

	// The proxy goes offline and drops out of the snapshot entirely. Its
	// earnings history is the whole point of this store and must NOT be
	// pruned the way the liveness tracker prunes its maps.
	s.Observe(map[string]*connect.ProxyBandwidth{"b:1080": bwWith(1)}, now)

	if got := s.Score("a:1080", now); got != 9000 {
		t.Fatalf("score after leaving snapshot = %v, want 9000", got)
	}
}

func TestEarningsSaveLoadRoundTripsAndDecays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earn.json")
	now := time.Now()

	s := newProxyEarningsStore(path)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(8000)}, now)
	if err := s.Save(now); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := newProxyEarningsStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Score("a:1080", now); got != 8000 {
		t.Fatalf("reloaded score = %v, want 8000", got)
	}
	// Decay is a function of wall time, so it applies across the restart.
	if got := reloaded.Score("a:1080", now.Add(earningsHalfLife)); got < 3980 || got > 4020 {
		t.Fatalf("reloaded score after one half-life = %v, want ~4000", got)
	}
}

func TestEarningsLoadOfMissingFileIsNotAnError(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "absent.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("Load of missing file = %v, want nil", err)
	}
}

func TestEarningsSaveEvictsLowestScorersBeyondTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earn.json")
	now := time.Now()
	s := newProxyEarningsStore(path)
	s.maxEntries = 3

	// Four proxies with distinct scores. Establish baselines, then earn.
	base := map[string]*connect.ProxyBandwidth{}
	for _, a := range []string{"a:1", "b:1", "c:1", "d:1"} {
		base[a] = bwWith(0)
	}
	s.Observe(base, now)
	s.Observe(map[string]*connect.ProxyBandwidth{
		"a:1": bwWith(10), "b:1": bwWith(20), "c:1": bwWith(30), "d:1": bwWith(40),
	}, now)

	if err := s.Save(now); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := newProxyEarningsStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Score("a:1", now); got != 0 {
		t.Fatalf("lowest scorer survived eviction with score %v, want evicted", got)
	}
	for _, a := range []string{"b:1", "c:1", "d:1"} {
		if got := reloaded.Score(a, now); got == 0 {
			t.Fatalf("%s was evicted, want retained", a)
		}
	}
}

func TestEarningsNormalizesFormattedSnapshotKeys(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	// The health snapshot keys by "proxy[N] (addr)"; callers ask by raw
	// address. Without normalization the score is unreachable.
	s.Observe(map[string]*connect.ProxyBandwidth{"proxy[7] (a:1080)": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"proxy[7] (a:1080)": bwWith(600)}, now)

	if got := s.Score("a:1080", now); got != 600 {
		t.Fatalf("score by raw address = %v, want 600", got)
	}
}

func TestEarningsMaybeSaveThrottlesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earn.json")
	s := newProxyEarningsStore(path)
	now := time.Now()

	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"a:1080": bwWith(7000)}, now)

	if !s.MaybeSave(now) {
		t.Fatal("first MaybeSave did not write")
	}
	if s.MaybeSave(now.Add(earningsSaveInterval / 2)) {
		t.Fatal("MaybeSave wrote again inside the interval")
	}
	if !s.MaybeSave(now.Add(earningsSaveInterval + time.Second)) {
		t.Fatal("MaybeSave did not write after the interval elapsed")
	}
}

func TestEarningsMaybeSaveIsANoOpWithoutAPath(t *testing.T) {
	s := newProxyEarningsStore("")
	if s.MaybeSave(time.Now()) {
		t.Fatal("MaybeSave wrote with no configured path")
	}
}

// creditEarningsAt gives addr a score of exactly bytes as of now, bypassing
// the snapshot baseline the delta path requires. The clock is explicit so a
// decay between setup and assertion cannot make a test flaky.
func creditEarningsAt(s *proxyEarningsStore, addr string, bytes float64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[addr] = &proxyEarningsEntry{Score: bytes, Updated: now}
}

func withGlobalEarningsStore(t *testing.T) func() {
	t.Helper()
	orig := globalProxyEarningsStore
	globalProxyEarningsStore = newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	return func() { globalProxyEarningsStore = orig }
}

func TestEarningsHistorySummaryCountsAndRanksTopEarner(t *testing.T) {
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	creditEarningsAt(globalProxyEarningsStore, "quiet", 10, now)
	creditEarningsAt(globalProxyEarningsStore, "biggest", 4_000_000_000, now)

	proxies := []*connect.ProxySettings{
		{Address: "quiet"},
		{Address: "biggest"},
		{Address: "never-earned"},
	}

	ranked, topAddr, topScore := earningsHistorySummary(proxies, now)

	if ranked != 2 {
		t.Errorf("ranked = %d, want 2 (the third proxy has no history)", ranked)
	}
	if topAddr != "biggest" {
		t.Errorf("topAddr = %q, want biggest", topAddr)
	}
	if topScore != 4_000_000_000 {
		t.Errorf("topScore = %v, want 4e9", topScore)
	}
}

func TestEarningsHistorySummaryOnAFreshNode(t *testing.T) {
	restore := withGlobalEarningsStore(t)
	defer restore()

	proxies := []*connect.ProxySettings{{Address: "a"}, {Address: "b"}}

	ranked, topAddr, topScore := earningsHistorySummary(proxies, time.Now())
	if ranked != 0 || topAddr != "" || topScore != 0 {
		t.Fatalf("fresh node summary = (%d, %q, %v), want (0, \"\", 0)", ranked, topAddr, topScore)
	}
}

// The earnings record must survive a proxy leaving the snapshot, but the
// delta baseline must not: prevCum gains an entry for every address ever
// seen, and nothing evicts it. On a node churning thousands of URL-sourced
// addresses over weeks of uptime that map grows without bound.
//
// perProxyEarnTracker prunes both its maps for exactly this reason. This
// store keeps its history and prunes only the baseline.
func TestEarningsBaselineDoesNotGrowWithChurn(t *testing.T) {
	s := newProxyEarningsStore(filepath.Join(t.TempDir(), "earn.json"))
	now := time.Now()

	// One long-lived proxy earns, then a thousand short-lived addresses
	// pass through, one per tick, the way a churning URL source behaves.
	s.Observe(map[string]*connect.ProxyBandwidth{"keeper:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"keeper:1080": bwWith(5000)}, now)

	for i := 0; i < 1000; i++ {
		addr := fmt.Sprintf("churn-%d:1080", i)
		s.Observe(map[string]*connect.ProxyBandwidth{
			"keeper:1080": bwWith(5000),
			addr:          bwWith(1),
		}, now)
	}

	s.mu.Lock()
	baselines := len(s.prevCum)
	s.mu.Unlock()

	// Only the live set from the last call should hold a baseline.
	if baselines > 2 {
		t.Errorf("prevCum holds %d baselines after 1000 churned addresses, want at most 2", baselines)
	}

	// The history itself must be untouched by that pruning.
	if got := s.Score("keeper:1080", now); got != 5000 {
		t.Errorf("keeper score = %v, want 5000; pruning the baseline must not touch the record", got)
	}
}

// Eviction ranks by score alone, so on a mature node a long-dead proxy
// holding a large decayed burst outranks a live proxy earning steadily.
// Dropping a LIVE proxy from the history is not merely a lost record: its
// baseline survives in prevCum, so the next tick creates a fresh entry
// worth one minute of traffic, its score is then tiny, and the next save
// evicts it again. The proxy is trapped resetting every save interval
// while dead proxies hold the ranking.
func TestEarningsSaveNeverEvictsALiveProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earn.json")
	now := time.Now()
	s := newProxyEarningsStore(path)
	s.maxEntries = 1

	// An offline proxy with a large historical score.
	creditEarningsAt(s, "offline-whale:1080", 900_000_000, now)

	// A live proxy earning modestly. Observing it makes it live: after the
	// baseline prune, prevCum holds exactly the live set.
	s.Observe(map[string]*connect.ProxyBandwidth{"live-earner:1080": bwWith(0)}, now)
	s.Observe(map[string]*connect.ProxyBandwidth{"live-earner:1080": bwWith(1000)}, now)

	if err := s.Save(now); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := s.Score("live-earner:1080", now); got == 0 {
		t.Error("live proxy was evicted; its record would reset every save interval")
	}
	if got := s.Score("offline-whale:1080", now); got != 0 {
		t.Errorf("offline proxy survived the cap at the live proxy's expense (score %v)", got)
	}
}
