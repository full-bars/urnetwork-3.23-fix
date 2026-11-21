package connect

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// bootstrapPlainDnsName restricts the fallback to the configured control domains.
func bootstrapPlainDnsName(settings *DohSettings, name string) bool {
	name = normalizeInternalDohName(name)
	if name == "" {
		return false
	}
	for _, domain := range settings.BootstrapPlainDnsDomains {
		domain = normalizeInternalDohName(domain)
		if domain != "" && (name == domain || strings.HasSuffix(name, "."+domain)) {
			return true
		}
	}
	return false
}

func bootstrapPlainDnsServers(settings *DohSettings, network string) []string {
	local := *settings
	if settings.DnsResolverSettings == nil || (len(settings.DnsResolverSettings.LocalDnsIpv4) == 0 && len(settings.DnsResolverSettings.LocalDnsIpv6) == 0) {
		local.DnsResolverSettings = DefaultDnsResolverSettings()
	}
	return dnsResolverAddrs(&local, false, network)
}

func newBootstrapPlainDnsLookup(settings *DohSettings, dialer *net.Dialer) func(context.Context, string, string) ([]net.IP, error) {
	next := new(atomic.Uint64)
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		servers := bootstrapPlainDnsServers(settings, network)
		if len(servers) == 0 {
			return nil, fmt.Errorf("no bootstrap DNS resolvers")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(dnsResolverAddrPick(ctx, servers, next), port))
	}}
	return rootedBootstrapLookup(resolver.LookupIP)
}

// rootedBootstrapLookup asks for the exact name. A name without a trailing dot
// goes through the host resolver's search list when it has fewer dots than
// ndots, so a search domain could answer for a protected control name. A rooted
// name is queried as written.
func rootedBootstrapLookup(lookup func(context.Context, string, string) ([]net.IP, error)) func(context.Context, string, string) ([]net.IP, error) {
	return func(ctx context.Context, network, host string) ([]net.IP, error) {
		if host != "" && !strings.HasSuffix(host, ".") {
			host += "."
		}
		return lookup(ctx, network, host)
	}
}
