package connect

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"testing"

	"github.com/go-playground/assert/v2"
)

// dohTestServer returns an httptest server serving Google-style JSON DoH
// responses (the same shape the production remote DoH servers speak). The
// responses are canned: the name is not resolved through the real network, so
// the test is hermetic and cannot hang on a slow/blocked resolver (the flake:
// the old test hit live 1.1.1.1/8.8.8.8/9.9.9.9 DoH servers and its retry
// loop could spin for the full query timeout each iteration).
func dohTestServer() *httptest.Server {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		resp := DohResponse{
			Status: 0,
			Answer: []DohAnswer{
				{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"},
				{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "10.10.10.10"},
			},
		}
		w.Header().Set("Content-Type", "application/dns-json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return server
}

// TestDohQuery is hermetic: it queries a local httptest DoH server, never the
// live network. Regression for the flake where this test hit real public DoH
// servers and hung when the network was slow (observed on CI and test boxes).
func TestDohQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := dohTestServer()
	defer server.Close()

	settings := DefaultDohSettings()
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: server.URL, Format: DohFormatJson},
	}

	testIp1, err := netip.ParseAddr("1.1.1.1")
	assert.Equal(t, err, nil)
	testIp2, err := netip.ParseAddr("10.10.10.10")
	assert.Equal(t, err, nil)

	for range 10 {
		ips := DohQuery(ctx, 4, "A", settings, "test1.bringyour.com")
		if len(ips) == 0 {
			// Should not happen against the local server; fail fast instead
			// of the old retry-forever loop.
			t.Fatal("DohQuery returned no answers against the local test server")
		}
		assert.Equal(t, len(ips), 2)
		ttl1 := ips[testIp1]
		assert.NotEqual(t, ttl1, 0)
		ttl2 := ips[testIp2]
		assert.NotEqual(t, ttl2, 0)
	}

}

func TestDohCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := dohTestServer()
	defer server.Close()

	settings := DefaultDohSettings()
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: server.URL, Format: DohFormatJson},
	}

	dohCache := NewDohCache(settings)

	testIp1, err := netip.ParseAddr("1.1.1.1")
	assert.Equal(t, err, nil)
	testIp2, err := netip.ParseAddr("10.10.10.10")
	assert.Equal(t, err, nil)

	for range 10 {
		ips := dohCache.Query(ctx, "A", "test1.bringyour.com")
		if len(ips) == 0 {
			t.Fatal("DohCache returned no answers against the local test server")
		}
		assert.Equal(t, len(ips), 2)
		if !slices.Contains(ips, testIp1) {
			t.Fatalf("DohCache answers %v missing %v", ips, testIp1)
		}
		if !slices.Contains(ips, testIp2) {
			t.Fatalf("DohCache answers %v missing %v", ips, testIp2)
		}
	}

}

// TestDohStaggeredLaunch verifies P1-1 + P1-2: the fan-out launches the first
// server immediately and only fires later servers after DohServerStagger, and a
// winning answer stops further launches. fast server answers <1ms; slow server
// sleeps 2s. With 500ms stagger the slow server must NOT be hit.
func TestDohStaggeredLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fastHits, slowHits atomic.Int32

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastHits.Add(1)
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"}},
		})
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowHits.Add(1)
		time.Sleep(2 * time.Second)
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "2.2.2.2"}},
		})
	}))
	defer slow.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 500 * time.Millisecond
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: fast.URL, Format: DohFormatJson},
		{Url: slow.URL, Format: DohFormatJson},
	}

	testIp, _ := netip.ParseAddr("1.1.1.1")
	ips := DohQuery(ctx, 4, "A", settings, "staggered.bringyour.com")
	if len(ips) == 0 {
		t.Fatal("staggered query returned no answers")
	}
	if _, ok := ips[testIp]; !ok {
		t.Fatalf("staggered query answers %v missing winning ip %v", ips, testIp)
	}
	time.Sleep(50 * time.Millisecond)
	if slowHits.Load() != 0 {
		t.Fatalf("slow server was hit %d times; staggered launch + stop-on-win should have prevented it", slowHits.Load())
	}
	if fastHits.Load() == 0 {
		t.Fatal("fast server was never hit")
	}
}

// TestDohStaggerDisabled verifies that with DohServerStagger=0 there is no delay
// between launches: when no answer wins early, every server is fired (both fast
// servers get hit). This contrasts with TestDohStaggeredLaunch, where a 750ms
// stagger lets the fast winner stop the slow server from ever launching.
func TestDohStaggerDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fastHits, slowHits atomic.Int32

	// both servers answer immediately so neither wins before the launcher loop
	// completes — proving the all-at-once (no-delay) launch path
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastHits.Add(1)
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"}},
		})
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowHits.Add(1)
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "2.2.2.2"}},
		})
	}))
	defer slow.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 0
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: fast.URL, Format: DohFormatJson},
		{Url: slow.URL, Format: DohFormatJson},
	}

	DohQuery(ctx, 4, "A", settings, "nostagger.bringyour.com")
	if fastHits.Load() == 0 {
		t.Fatal("fast server was never hit")
	}
	if slowHits.Load() == 0 {
		t.Fatal("slow server was not launched with stagger disabled (expected all-at-once)")
	}
}

// TestDohSingleFlight verifies P1-3: concurrent identical queries coalesce onto
// one resolution (single-flight). A burst of 16 concurrent identical queries
// must hit the underlying DoH server exactly once.
func TestDohSingleFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"}},
		})
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: server.URL, Format: DohFormatJson},
	}
	dohCache := NewDohCache(settings)

	const n = 16
	var wg sync.WaitGroup
	results := make([][]netip.Addr, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = dohCache.Query(ctx, "A", "coalesce.bringyour.com")
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if len(r) == 0 {
			t.Fatalf("caller %d got no answer", i)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 DoH server hit (single-flight), got %d", got)
	}
}

// --- P2-2: server scoring tests ---

// TestServerStatsTokenBucket verifies the sliding-window counter: events in the
// current interval count fully, events in the previous interval prorate by
// overlap, and a gap clears both.
func TestServerStatsTokenBucket(t *testing.T) {
	span := 10 * time.Second
	now := time.Unix(1000, 0)

	b := tokenBucket{}
	b.add(span, now, 3) // nothing before -> 3 in current
	if got := b.estimate(span, now); got != 3 {
		t.Fatalf("after single add: want 3, got %v", got)
	}

	// advance exactly one span: previous becomes the old current (3), new current 0
	next := now.Add(span)
	if got := b.estimate(span, next); got != 3 {
		t.Fatalf("one span later: previous should carry 3, got %v", got)
	}
	b.add(span, next, 2) // add 2 to the new current window
	if got := b.estimate(span, next); got != 5 {
		t.Fatalf("after second add: want 5, got %v", got)
	}

	// advance half a span into the third window: previous (2) prorates by 0.5
	half := next.Add(5 * time.Second)
	got := b.estimate(span, half)
	want := 2.0*0.5 + 0.0 // previous=2 prorated to 1.0, current=0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("half-span prorate: want %v, got %v", want, got)
	}

	// advance far beyond (gap > 2 spans): both windows must clear
	far := now.Add(100 * span)
	if got := b.estimate(span, far); got != 0 {
		t.Fatalf("after large gap: want 0, got %v", got)
	}
}

// TestServerStatsRecordAndScore verifies that recorded successes raise a
// server's score and that an untried server scores 0.
func TestServerStatsRecordAndScore(t *testing.T) {
	s := newServerStats()
	if got := s.scoreLocked("https://1.1.1.1/dns-query", time.Now()); got != 0 {
		t.Fatalf("untried server should score 0, got %v", got)
	}
	s.record("https://1.1.1.1/dns-query", true)
	s.record("https://1.1.1.1/dns-query", true)
	s.record("https://1.1.1.1/dns-query", true)
	score := s.scoreLocked("https://1.1.1.1/dns-query", time.Now())
	if score <= 0 {
		t.Fatalf("after 3 successes want positive score, got %v", score)
	}
	// failures must not increase the score
	before := s.scoreLocked("https://1.1.1.1/dns-query", time.Now())
	s.record("https://1.1.1.1/dns-query", false)
	after := s.scoreLocked("https://1.1.1.1/dns-query", time.Now())
	if after != before {
		t.Fatalf("failure should not change score: before %v after %v", before, after)
	}
	// scores() should export the positive server and omit zero-score ones
	exported := s.scores()
	if _, ok := exported["https://1.1.1.1/dns-query"]; !ok {
		t.Fatal("scores() omitted a server that has a positive score")
	}
}

// TestServerStatsOrderBias verifies the weighted permutation favors high-score
// servers: across many samples, the better server should lead strictly more
// often than the floor-only (untried) server.
func TestServerStatsOrderBias(t *testing.T) {
	good := "https://1.1.1.1/dns-query"
	bad := "https://8.8.8.8/resolve"

	s := newServerStats()
	// credit the good server heavily across all windows
	now := time.Now()
	for k, span := range dohServerWindows {
		s.byUrl[good].windows[k].add(span, now, 8)
	}
	// bad server is untried -> only the 0.05 exploration floor

	const samples = 2000
	goodFirst := 0
	for i := 0; i < samples; i++ {
		ordered := s.order([]string{good, bad})
		if ordered[0] == good {
			goodFirst++
		}
	}
	// with a score of ~8 vs floor 0.05, good should lead >99% of the time
	if goodFirst < samples*95/100 {
		t.Fatalf("weighted order bias too weak: good led %d/%d times", goodFirst, samples)
	}
}

// TestServerStatsOrderUniformWhenNil verifies that a nil *serverStats yields a
// uniform-random (unbiased) shuffle, not a fixed order.
func TestServerStatsOrderUniformWhenNil(t *testing.T) {
	var s *serverStats
	urls := []string{"a", "b", "c", "d"}
	aFirst := 0
	const samples = 4000
	for i := 0; i < samples; i++ {
		if s.order(urls)[0] == "a" {
			aFirst++
		}
	}
	// uniform over 4 items => a leads ~25%; allow wide margin
	if aFirst < samples/10 || aFirst > samples*4/10 {
		t.Fatalf("nil stats order not uniform: a led %d/%d", aFirst, samples)
	}
}

// TestServerStatsSeedRoundTrip verifies seed() loads scores and scores()
// exports them (the provider persistence path: save scores, restart, reseed).
func TestServerStatsSeedRoundTrip(t *testing.T) {
	s := newServerStats()
	seed := map[string]float64{
		"https://1.1.1.1/dns-query": 6.0,
		"https://9.9.9.9/dns-query":  2.0,
	}
	s.seed(seed)

	// seeded scores must be present and clamped (never exceed dohSeedMaxScore)
	got := s.scores()
	if math.Abs(got["https://1.1.1.1/dns-query"]-6.0) > 1e-6 {
		t.Fatalf("seed score not loaded: got %v want 6", got["https://1.1.1.1/dns-query"])
	}
	// round-trip: a fresh stats seeded from scores() must reproduce ordering
	s2 := newServerStats()
	s2.seed(s.scores())
	if math.Abs(s2.scoreLocked("https://1.1.1.1/dns-query", time.Now())-6.0) > 1e-6 {
		t.Fatal("seed round-trip lost the score")
	}

	// over-seeded scores are clamped
	s3 := newServerStats()
	s3.seed(map[string]float64{"https://1.1.1.1/dns-query": 999})
	if s3.scoreLocked("https://1.1.1.1/dns-query", time.Now()) > dohSeedMaxScore+1e-9 {
		t.Fatal("seed did not clamp to dohSeedMaxScore")
	}
}

// TestDohCacheServerScoresLive verifies that a live cache records successes
// and exposes them via ServerScores() (the value the provider persists).
func TestDohCacheServerScoresLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"}},
		})
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: server.URL, Format: DohFormatJson},
	}
	cache := NewDohCache(settings)
	cache.Query(ctx, "A", "live-score.bringyour.com")

	exported := cache.ServerScores()
	if _, ok := exported[server.URL]; !ok {
		t.Fatal("ServerScores() did not contain the server that just succeeded")
	}
	if exported[server.URL] <= 0 {
		t.Fatalf("ServerScores() returned non-positive score for successful server: %v", exported[server.URL])
	}
}

// TestDohStatsSeededOrdering verifies that seeding ServerStatsSeed at
// construction makes the seeded server lead the fan-out (drives first pick
// after restart instead of uniform-random), and that a live query records the
// winner under its OWN URL (regression guard for the per-goroutine server
// capture — a closure over the loop variable would mis-attribute a result to
// the wrong server).
func TestDohStatsSeededOrdering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "1.1.1.1"}},
		})
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 300, Data: "2.2.2.2"}},
		})
	}))
	defer slow.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 0 // all-at-once so both servers are actually hit
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: fast.URL, Format: DohFormatJson},
		{Url: slow.URL, Format: DohFormatJson},
	}
	// seed slow as the clear winner so it leads the fan-out
	settings.ServerStatsSeed = map[string]float64{slow.URL: 8.0}

	cache := NewDohCache(settings)

	// the seeded server must lead the ordering
	ordered := cache.stats.order([]string{fast.URL, slow.URL})
	if ordered[0] != slow.URL {
		t.Fatalf("seeded server should lead ordering, got %v", ordered)
	}

	// a live query records the winner under its own URL; it must never be
	// attributed to the other server's URL (closure-capture regression)
	cache.Query(ctx, "A", "seeded.bringyour.com")
	exported := cache.ServerScores()
	if len(exported) < 1 {
		t.Fatal("expected at least the winning server to be scored")
	}
	for url := range exported {
		if url != fast.URL && url != slow.URL {
			t.Fatalf("ServerScores() contains a mis-attributed URL: %s", url)
		}
	}
}

// TestDohServeStale verifies RFC 8767 serve-stale: once a record is cached and
// its TTL expires, a live resolution failure still returns the stale address
// (so client traffic survives a resolver outage) instead of an empty/SERVFAIL.
func TestDohServeStale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// resolver that returns 1.1.1.1 for the first call, then fails hard
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			json.NewEncoder(w).Encode(DohResponse{
				Status: 0,
				Answer: []DohAnswer{{Name: r.URL.Query().Get("name"), Type: 1, TTL: 1, Data: "1.1.1.1"}},
			})
			return
		}
		// second call: resolver is down
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 2 * time.Second
	settings.MinCacheTtl = 0 // let the 1s TTL actually expire so we can test serve-stale
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableLocalDoh = false
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: server.URL, Format: DohFormatJson},
	}
	cache := NewDohCache(settings)

	// prime: fresh resolution caches 1.1.1.1 (TTL 1s)
	addrs, authoritative := cache.QueryResult(ctx, "A", "stale.bringyour.com")
	if !authoritative || len(addrs) != 1 {
		t.Fatalf("prime query: want authoritative 1 addr, got auth=%v addrs=%v", authoritative, addrs)
	}

	// let the cached record expire (TTL 1s)
	time.Sleep(1200 * time.Millisecond)

	// live resolution now fails (resolver returns 500) -> must serve stale
	staleAddrs, staleAuth := cache.QueryResult(ctx, "A", "stale.bringyour.com")
	if len(staleAddrs) != 1 {
		t.Fatalf("serve-stale: expected 1 stale address, got %v (auth=%v)", staleAddrs, staleAuth)
	}
	if staleAuth {
		t.Fatal("serve-stale answer must be marked non-authoritative (auth=false)")
	}
	if staleAddrs[0].String() != "1.1.1.1" {
		t.Fatalf("serve-stale returned wrong address: %v", staleAddrs)
	}
	if got := cache.staleServeCount.Load(); got == 0 {
		t.Fatal("staleServeCount should be > 0 after a served-stale lookup")
	}
}
