package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"

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
