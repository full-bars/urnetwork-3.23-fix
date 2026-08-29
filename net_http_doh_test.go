package connect

import (
	"context"
	"encoding/json"
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

// TestDohStaggerDisabled preserves the old behavior: with DohServerStagger=0,
// every server is launched at once (both fast and slow servers get hit).
func TestDohStaggerDisabled(t *testing.T) {
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
		t.Fatal("slow server was not launched with stagger disabled")
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
