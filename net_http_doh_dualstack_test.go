package connect

// net_http_doh_dualstack_test.go — the DoH resolver's address-family policy
// and its round-robin server choice.
//
// Ported from connect v0.0.0-20261017075543-c8d8cdf536a8
// (net_http_doh_dualstack_test.go), adapted to this tree's DohServer type (a
// url plus the query format it speaks, not a bare url string).
//
// The test this replaces, TestDnsResolverAddrsOmitIpv6WithoutHostIpv6, pinned
// the older `if !hostHasGlobalIpv6() { ipv6 = nil }` filter: a v6 resolver was
// never offered on a host whose one-shot v6 probe failed. That filter is gone
// (see dnsResolverAddrs): the family a lookup needs is decided per query by the
// record type (dnsLookupNetwork), and a family the path lacks costs one
// attempt out of an operator-interleaved list rather than suppressing the other
// family for the life of the process.
//
// The PIN's launch-wave grouping (dohServerSiblings/dohLaunchWaves) is NOT
// ported: this tree's fan-out is the stagger in dohQueryWithClientResult, which
// has no per-operator wave concept. The operator pairing it consumes is ported
// (familySiblings), so a later port can build on it.

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func withHostIpv6(t *testing.T, has bool) {
	old := hostHasGlobalIpv6
	hostHasGlobalIpv6 = func() bool { return has }
	t.Cleanup(func() { hostHasGlobalIpv6 = old })
}

func TestDohIpVersionFollowsHostIpv6(t *testing.T) {
	withHostIpv6(t, true)
	if v := DefaultDohSettings().IpVersion; v != 0 {
		t.Fatalf("dual-stack host: IpVersion %d, want 0", v)
	}
	withHostIpv6(t, false)
	if v := DefaultDohSettings().IpVersion; v != 4 {
		t.Fatalf("IPv4-only host must keep IpVersion 4, got %d", v)
	}
}

func TestDefaultIpv6DohServersAreWellFormed(t *testing.T) {
	rs := DefaultDnsResolverSettings()
	if len(rs.RemoteDohServersIpv6) != len(rs.RemoteDohServersIpv4) {
		t.Fatalf("each operator needs a v6 endpoint: %d v4, %d v6", len(rs.RemoteDohServersIpv4), len(rs.RemoteDohServersIpv6))
	}
	for _, s := range rs.RemoteDohServersIpv6 {
		u, err := url.Parse(s.Url)
		if err != nil || u.Scheme != "https" {
			t.Fatalf("bad v6 doh url %q: %v", s.Url, err)
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || ip.To4() != nil {
			t.Fatalf("v6 doh server %q must be an IPv6 literal", s.Url)
		}
		if s.Format != DohFormatJson && s.Format != DohFormatWire {
			t.Fatalf("v6 doh server %q has no format", s.Url)
		}
	}
	for _, a := range rs.LocalDnsIpv6 {
		if ip := net.ParseIP(a); ip == nil || ip.To4() != nil {
			t.Fatalf("local v6 dns %q is not IPv6", a)
		}
	}
}

func TestDualStackSettingsConsultBothFamilies(t *testing.T) {
	withHostIpv6(t, true)
	st := DefaultDohSettings()
	servers := remoteDohServers(st, st.IpVersion)
	var v4, v6 int
	for _, s := range servers {
		if strings.Contains(s.Url, "[") {
			v6++
		} else {
			v4++
		}
	}
	if v4 == 0 || v6 == 0 {
		t.Fatalf("dual-stack must use both families, got v4=%d v6=%d", v4, v6)
	}
	addrs := dnsResolverAddrs(st, false, "")
	var has6 bool
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() == nil {
			has6 = true
		}
	}
	if !has6 {
		t.Fatalf("local dns addrs missing IPv6: %v", addrs)
	}
	// a v4-only host must not see any v6 server
	withHostIpv6(t, false)
	st = DefaultDohSettings()
	for _, s := range remoteDohServers(st, st.IpVersion) {
		if strings.Contains(s.Url, "[") {
			t.Fatalf("IPv4-only host would consult a v6 doh server: %s", s.Url)
		}
	}
}

func TestDetectGlobalIpv6DoesNotPanicOrBlock(t *testing.T) {
	_ = detectGlobalIpv6() // result depends on the host; it must return promptly
}

// The shipped defaults carry both families: every operator's v6 endpoint sits
// beside its v4 one, in the known-pair table, and the cache consults both.
func TestDefaultDnsResolverSettingsCarryIpv6(t *testing.T) {
	rs := DefaultDnsResolverSettings()
	if len(rs.RemoteDohServersIpv6) != len(rs.RemoteDohServersIpv4) {
		t.Fatalf("remote doh v6 servers = %d, want one per v4 operator (%d)", len(rs.RemoteDohServersIpv6), len(rs.RemoteDohServersIpv4))
	}
	if len(rs.RemoteDnsIpv6) != len(rs.RemoteDnsIpv4) {
		t.Fatalf("remote dns v6 = %d, want one per v4 operator (%d)", len(rs.RemoteDnsIpv6), len(rs.RemoteDnsIpv4))
	}
	if len(rs.LocalDnsIpv6) != len(rs.LocalDnsIpv4) {
		t.Fatalf("local dns v6 = %d, want one per v4 operator (%d)", len(rs.LocalDnsIpv6), len(rs.LocalDnsIpv4))
	}
	for _, addr := range slices.Concat(rs.RemoteDnsIpv6, rs.LocalDnsIpv6) {
		if parsed, err := netip.ParseAddr(addr); err != nil || !parsed.Is6() {
			t.Fatalf("dns v6 entry %q is not a v6 literal", addr)
		}
	}
	siblings := familySiblings(
		dohServerUrls(rs.RemoteDohServersIpv4),
		dohServerUrls(rs.RemoteDohServersIpv6),
		knownDohUrlPairs,
	)
	for _, server := range rs.RemoteDohServersIpv4 {
		if _, ok := siblings[server.Url]; !ok {
			t.Fatalf("default doh url %s has no v6 sibling in the known-pair table", server.Url)
		}
	}
	// this tree keeps the host-dependent default, so pin it against the probe
	// rather than against a literal
	if got := DefaultDohSettings().IpVersion; got != defaultDohIpVersion() {
		t.Fatalf("DefaultDohSettings().IpVersion = %d, want defaultDohIpVersion() = %d", got, defaultDohIpVersion())
	}
	settings := DefaultDohSettings()
	settings.IpVersion = 0
	if got := settings.ResolverIp(); got != "ip" {
		t.Fatalf("ResolverIp() = %s, want ip", got)
	}
}

// With IpVersion 0 the defaults produce an operator-interleaved list. A
// leftover v6 entry from the shipped table drops out when its v4 partner is
// replaced, while a leftover entry the CALLER configured is always kept.
func TestDohServersForDualStackPairsOperators(t *testing.T) {
	rs := DefaultDnsResolverSettings()
	servers := dohServersFor(rs.RemoteDohServersIpv4, rs.RemoteDohServersIpv6, 0)
	if len(servers) != len(rs.RemoteDohServersIpv4)+len(rs.RemoteDohServersIpv6) {
		t.Fatalf("dual-stack servers = %v, want every default of both families", dohServerUrls(servers))
	}
	for i := 0; i+1 < len(servers); i += 2 {
		if !strings.Contains(servers[i].Url, "https://[") && strings.Contains(servers[i+1].Url, "https://[") {
			continue
		}
		t.Fatalf("servers[%d..%d] = %v, want v4 followed by its v6 sibling",
			i, i+1, dohServerUrls(servers[i:i+2]))
	}
	// the format tag travels with its url through the interleave
	byUrl := map[string]DohFormat{}
	for _, s := range rs.RemoteDohServersIpv4 {
		byUrl[s.Url] = s.Format
	}
	for _, s := range rs.RemoteDohServersIpv6 {
		byUrl[s.Url] = s.Format
	}
	for _, s := range servers {
		if byUrl[s.Url] != s.Format {
			t.Fatalf("interleaved %s carries format %q, want %q", s.Url, s.Format, byUrl[s.Url])
		}
	}

	// replacing the v4 list means "use my servers": the shipped v6 defaults are
	// known-table entries whose v4 partners are now absent, so they drop out
	custom := DohServer{Url: "http://127.0.0.1:1/dns-query", Format: DohFormatJson}
	only := dohServersFor([]DohServer{custom}, rs.RemoteDohServersIpv6, 0)
	if len(only) != 1 || only[0].Url != custom.Url {
		t.Fatalf("a replaced v4 list resolved to %v, want only the custom server", dohServerUrls(only))
	}

	// a v6-heavy CUSTOM configuration keeps every surplus server: those can
	// only have come from the caller, and familySiblings can index-pair only
	// min(len4, len6) of them
	heavy := dohServersFor(
		[]DohServer{custom},
		[]DohServer{
			{Url: "http://[::1]:1/dns-query"},
			{Url: "http://[::2]:2/dns-query"},
			{Url: "http://[::3]:3/dns-query"},
		},
		0,
	)
	wantHeavy := []string{
		custom.Url,
		"http://[::1]:1/dns-query",
		"http://[::2]:2/dns-query",
		"http://[::3]:3/dns-query",
	}
	if got := dohServerUrls(heavy); !slices.Equal(got, wantHeavy) {
		t.Fatalf("v6-heavy list = %v, want %v (no caller server dropped)", got, wantHeavy)
	}

	// a custom pair outside the table pairs by index
	custom6 := DohServer{Url: "http://[::1]:1/dns-query"}
	pair := dohServersFor([]DohServer{custom}, []DohServer{custom6}, 0)
	if got := dohServerUrls(pair); !slices.Equal(got, []string{custom.Url, custom6.Url}) {
		t.Fatalf("custom pair = %v, want [%s %s]", got, custom.Url, custom6.Url)
	}

	// a pure v6 configuration is used as is
	if got := dohServersFor(nil, rs.RemoteDohServersIpv6, 0); !slices.Equal(dohServerUrls(got), dohServerUrls(rs.RemoteDohServersIpv6)) {
		t.Fatalf("pure v6 = %v, want the v6 list", dohServerUrls(got))
	}
	// explicit versions keep their one list
	if got := dohServersFor(rs.RemoteDohServersIpv4, rs.RemoteDohServersIpv6, 4); !slices.Equal(dohServerUrls(got), dohServerUrls(rs.RemoteDohServersIpv4)) {
		t.Fatalf("ipVersion 4 = %v, want the v4 list", dohServerUrls(got))
	}
	if got := dohServersFor(rs.RemoteDohServersIpv4, rs.RemoteDohServersIpv6, 6); !slices.Equal(dohServerUrls(got), dohServerUrls(rs.RemoteDohServersIpv6)) {
		t.Fatalf("ipVersion 6 = %v, want the v6 list", dohServerUrls(got))
	}
}

func TestFamilySiblingsPairsKnownAndCustomEntries(t *testing.T) {
	siblings := familySiblings(
		[]string{"1.1.1.1", "custom4", "8.8.8.8"},
		[]string{"2620:fe::fe", "custom6", "2606:4700:4700::1111"},
		knownDnsServerPairs,
	)
	want := map[string]string{
		"1.1.1.1":              "2606:4700:4700::1111",
		"2606:4700:4700::1111": "1.1.1.1",
		"custom4":              "custom6",
		"custom6":              "custom4",
	}
	if len(siblings) != len(want) {
		t.Fatalf("siblings = %v, want %v", siblings, want)
	}
	for k, v := range want {
		if siblings[k] != v {
			t.Fatalf("siblings[%s] = %s, want %s", k, siblings[k], v)
		}
	}
	// 8.8.8.8's partner is absent and 2620:fe::fe's partner is absent: neither pairs
}

func TestDnsResolverAddrsDualStackInterleavesOperators(t *testing.T) {
	settings := DefaultDohSettings()
	settings.IpVersion = 0
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDns: true,
		RemoteDnsIpv4:   []string{"1.1.1.1", "8.8.8.8"},
		RemoteDnsIpv6:   []string{"2606:4700:4700::1111", "2001:4860:4860::8888"},
	}
	rs := settings.DnsResolverSettings
	addrs := dnsResolverAddrs(settings, true, "udp")
	if len(addrs) != len(rs.RemoteDnsIpv4)+len(rs.RemoteDnsIpv6) {
		t.Fatalf("addrs = %v, want both families", addrs)
	}
	if addrs[0] != rs.RemoteDnsIpv4[0] || addrs[1] != knownSibling(rs.RemoteDnsIpv4[0]) {
		t.Fatalf("addrs[0..1] = %v, want the first operator's v4 then v6", addrs[:2])
	}
	if addrs[2] != rs.RemoteDnsIpv4[1] || addrs[3] != knownSibling(rs.RemoteDnsIpv4[1]) {
		t.Fatalf("addrs[2..3] = %v, want the second operator's v4 then v6", addrs[2:])
	}
	if got := dnsResolverAddrs(settings, true, "udp4"); !slices.Equal(got, rs.RemoteDnsIpv4) {
		t.Fatalf("udp4 = %v, want the v4 list", got)
	}
	if got := dnsResolverAddrs(settings, true, "udp6"); !slices.Equal(got, rs.RemoteDnsIpv6) {
		t.Fatalf("udp6 = %v, want the v6 list", got)
	}
	if got := dnsResolverAddrs(settings, true, ""); !slices.Equal(got, addrs) {
		t.Fatalf("a family-agnostic lookup = %v, want the interleaved list %v", got, addrs)
	}
}

func knownSibling(entry string) string {
	for _, pair := range knownDnsServerPairs {
		if pair[0] == entry {
			return pair[1]
		}
	}
	return ""
}

func TestDnsLookupNetworkFollowsTheRecordType(t *testing.T) {
	if network, ok := dnsLookupNetwork("A", 0); !ok || network != "ip4" {
		t.Fatalf("A/0 = %s,%t want ip4,true", network, ok)
	}
	if network, ok := dnsLookupNetwork("AAAA", 0); !ok || network != "ip6" {
		t.Fatalf("AAAA/0 = %s,%t want ip6,true", network, ok)
	}
	if _, ok := dnsLookupNetwork("AAAA", 4); ok {
		t.Fatal("AAAA under IpVersion 4 must be skipped, not answered with v4 records")
	}
	if _, ok := dnsLookupNetwork("A", 6); ok {
		t.Fatal("A under IpVersion 6 must be skipped")
	}
	if _, ok := dnsLookupNetwork("HTTPS", 0); ok {
		t.Fatal("an opaque record type has no plain-dns address lookup")
	}
}

// The plain-dns server choice is a ROUND ROBIN over the family-filtered list,
// not a random pick: over an operator-interleaved list a random choice can land
// on the same unreachable family on every retry of the same name, and each miss
// costs the caller a full resolver timeout.
func TestDnsResolverAddrPickRotatesInOrder(t *testing.T) {
	addrs := []string{"1.1.1.1", "2606:4700:4700::1111", "8.8.8.8"}
	var next atomic.Uint64
	want := []string{
		"1.1.1.1",
		"2606:4700:4700::1111",
		"8.8.8.8",
		"1.1.1.1",
		"2606:4700:4700::1111",
	}
	for i, expected := range want {
		if got := dnsResolverAddrPick(addrs, &next); got != expected {
			t.Fatalf("pick %d = %s, want %s", i, got, expected)
		}
	}
	// a single configured server is dialed every time
	var single atomic.Uint64
	for i := 0; i < 3; i += 1 {
		if got := dnsResolverAddrPick([]string{"9.9.9.9"}, &single); got != "9.9.9.9" {
			t.Fatalf("single pick = %s, want 9.9.9.9", got)
		}
	}
}

// End to end over the real resolver the cache dials: consecutive remote
// resolver attempts walk the operator-interleaved list and wrap.
func TestDohResolverRoundRobinWalksTheInterleavedList(t *testing.T) {
	settings := DefaultDohSettings()
	settings.IpVersion = 0
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDns: true,
		RemoteDnsIpv4:   []string{"1.1.1.1", "8.8.8.8"},
		RemoteDnsIpv6:   []string{"2606:4700:4700::1111", "2001:4860:4860::8888"},
	}
	var mutex sync.Mutex
	dialed := []string{}
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			mutex.Lock()
			dialed = append(dialed, addr)
			mutex.Unlock()
			return &stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 53}}, nil
		},
	}

	cache := NewDohCache(settings)
	defer cache.Close()

	want := []string{
		"1.1.1.1:53",
		"[2606:4700:4700::1111]:53",
		"8.8.8.8:53",
		"[2001:4860:4860::8888]:53",
	}
	for i := 0; i < 2*len(want); i += 1 {
		if _, err := cache.remoteResolver.Dial(context.Background(), "udp", "127.0.0.53:53"); err != nil {
			t.Fatal(err)
		}
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(dialed) != 2*len(want) {
		t.Fatalf("dialed %d resolvers, want %d: %v", len(dialed), 2*len(want), dialed)
	}
	for i, addr := range dialed {
		if addr != want[i%len(want)] {
			t.Fatalf("dial %d went to %s, want %s (all: %v)", i, addr, want[i%len(want)], dialed)
		}
	}
}

// Regional recommendations carry their operators' v6 endpoints too, so a
// region that publishes one is not silently v4-only under the dual-stack
// walk (dualStackList). A region with no published v6 keeps a v4-only
// recommendation, and the v4 side is unchanged by the port.
func TestRegionalDnsServersCarryIpv6(t *testing.T) {
	cn := RegionalDnsResolverSettings("cn")
	if cn == nil {
		t.Fatal("cn has no regional recommendation")
	}
	if len(cn.RemoteDnsIpv6) == 0 {
		t.Fatalf("cn regional settings carry no v6 servers: %v", cn)
	}
	for _, addr := range cn.RemoteDnsIpv6 {
		if parsed, err := netip.ParseAddr(addr); err != nil || !parsed.Is6() {
			t.Fatalf("cn v6 entry %q is not a v6 literal", addr)
		}
	}
	if !slices.Equal(RegionalDnsServerIpv6s("CN"), cn.RemoteDnsIpv6) {
		t.Fatalf("RegionalDnsServerIpv6s(CN) = %v, want %v", RegionalDnsServerIpv6s("CN"), cn.RemoteDnsIpv6)
	}
	// a region without any published v6 resolver keeps a v4-only recommendation
	ir := RegionalDnsResolverSettings("ir")
	if ir == nil || len(ir.RemoteDnsIpv4) == 0 || len(ir.RemoteDnsIpv6) != 0 {
		t.Fatalf("ir = %v, want v4 servers and no v6 servers", ir)
	}
	if RegionalDnsResolverSettings("zz") != nil {
		t.Fatal("unknown region must have no recommendation")
	}
	for _, server := range RegionalDnsServers() {
		if server.Ipv6 == "" {
			continue
		}
		if parsed, err := netip.ParseAddr(server.Ipv6); err != nil || !parsed.Is6() {
			t.Fatalf("%s %s v6 %q is not a v6 literal", server.CountryCode, server.Name, server.Ipv6)
		}
	}
	if got := RegionalDnsServerIps("cn"); !slices.Equal(got, []string{"223.5.5.5", "119.29.29.29"}) {
		t.Fatalf("cn v4 = %v, want the unchanged v4 list", got)
	}
}
