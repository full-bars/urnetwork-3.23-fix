package connect

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

type bootstrapStallTransport struct{ calls atomic.Int32 }

func (s *bootstrapStallTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func bootstrapTestCache(t *testing.T, reserve time.Duration) (*DohCache, *bootstrapStallTransport, *atomic.Int32) {
	t.Helper()
	settings := DefaultDohSettings()
	settings.IpVersion = 4
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 0
	settings.BootstrapPlainDnsDomains = []string{"space.example"}
	settings.BootstrapPlainDnsReserve = reserve
	settings.DnsResolverSettings = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{"https://192.0.2.1/dns-query"}}
	cache := NewDohCache(settings)
	t.Cleanup(cache.Close)
	transport := &bootstrapStallTransport{}
	cache.httpClient.Transport = transport
	calls := &atomic.Int32{}
	cache.bootstrapLookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		calls.Add(1)
		return []net.IP{net.ParseIP("192.0.2.81")}, nil
	}
	return cache, transport, calls
}

func TestBootstrapPlainDnsLeavesDohAtLeastTheReserve(t *testing.T) {
	cache, transport, calls := bootstrapTestCache(t, 250*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	addrs, _ := cache.QueryResult(ctx, "A", "connect.space.example")
	if len(addrs) != 0 || calls.Load() != 0 || transport.calls.Load() == 0 {
		t.Fatalf("addrs=%v fallback calls=%d DoH calls=%d", addrs, calls.Load(), transport.calls.Load())
	}
}

func TestBootstrapPlainDnsGetsShortDeadlineShare(t *testing.T) {
	cache, transport, calls := bootstrapTestCache(t, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	addrs, ok := cache.QueryResult(ctx, "A", "connect.space.example")
	if !ok || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("192.0.2.81") || calls.Load() != 1 || transport.calls.Load() == 0 {
		t.Fatalf("addrs=%v authoritative=%v fallback calls=%d DoH calls=%d", addrs, ok, calls.Load(), transport.calls.Load())
	}
}

func TestBootstrapStalledResolverReachesControlDial(t *testing.T) {
	cache, transport, calls := bootstrapTestCache(t, 100*time.Millisecond)
	resolver := &internalDohResolver{cache: cache, domains: []string{"space.example"}}
	dial := resolver.wrapDialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "192.0.2.81:443" {
			t.Errorf("dialed %s", address)
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	conn, err := dial(ctx, "tcp4", "connect.space.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	if transport.calls.Load() == 0 || calls.Load() != 1 {
		t.Fatalf("fallback calls=%d DoH calls=%d", calls.Load(), transport.calls.Load())
	}
}
