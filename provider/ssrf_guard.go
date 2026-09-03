package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// SSRF hardening for operator-supplied proxy-source URLs (both `proxy add-source`
// and `proxy paste` URL lines). These URLs may point anywhere, and the Go http
// client resolves the host and follows redirects by default — so a pasted/external
// URL could be made to dial loopback, link-local, or private RFC1918 addresses
// (e.g. a redirect to http://169.254.169.254/ or an internal service). We reject
// any destination resolving to a non-global IP at dial time AND on redirect.
//
// This intentionally matches (and tightens) the trust model: source URLs are
// operator-supplied for fetching public proxy lists, which are always global-routed.

// ssrfAllowLoopback allows loopback dials/redirects when true. This is used
// exclusively in unit tests to allow httptest.NewServer mock endpoints.
var ssrfAllowLoopback atomic.Bool

// isBlockedSourceIP reports whether ip must never be contacted by a fetcher.
// Blocks: loopback (127.0.0.0/8, ::1), link-local (169.254.0.0/16, fe80::/10),
// private RFC1918 (10/8, 172.16/12, 192.168/16) + IPv6 ULA (fc00::/7),
// unspecified, and multicast.
func isBlockedSourceIP(ip net.IP) bool {
	if ip == nil {
		return true // unresolvable — treat as unsafe
	}
	if ssrfAllowLoopback.Load() && ip.IsLoopback() {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// ssrfLookupHost resolves hostname and reports whether it maps to any
// blocked (non-global) address. A host resolving to a mix of public and
// private addresses is still rejected — the fetcher must only ever reach
// globally-routed IPs.
func ssrfLookupHost(host string) error {
	addrs, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if isBlockedSourceIP(a) {
			return fmt.Errorf("source URL host %q resolves to non-global address %s", host, a)
		}
	}
	return nil
}

// ssrfVerifyURLHost rejects a URL whose recorded host resolves only to blocked
// (non-global) addresses. host may be an IP literal or a hostname.
func ssrfVerifyURLHost(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("source URL %q has no host", rawURL)
	}
	return ssrfLookupHost(host)
}

// ssrfDialContext wraps a plain TCP dialer so any dial to a blocked
// (non-global) destination is refused immediately, before bytes are sent.
// http.Transport passes the still-unresolved hostname here for the common
// case (a hostname source URL) — resolution happens inside net.Dialer, not
// before this call — so we must resolve and validate the destination
// ourselves rather than only guarding the case where addr is already a
// literal IP.
func ssrfDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedSourceIP(ip) {
			return nil, fmt.Errorf("refusing dial to non-global address %s (SSRF guard)", addr)
		}
		d := net.Dialer{}
		return d.DialContext(ctx, network, addr)
	}

	resolver := net.Resolver{}
	ipAddrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{}
	var lastErr error
	for _, ipAddr := range ipAddrs {
		if isBlockedSourceIP(ipAddr.IP) {
			lastErr = fmt.Errorf("refusing dial to non-global address %s (SSRF guard, host %s)", ipAddr.IP, host)
			continue
		}
		conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses found for host %s", host)
	}
	return nil, lastErr
}

// ssrfCheckRedirect rejects any HTTP redirect whose target resolves to a
// blocked (non-global) address — a public-looking source URL must not be
// able to redirect the fetcher into loopback/link-local/private space.
func ssrfCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("stopped after 3 redirects")
	}
	return ssrfVerifyURLHost(req.URL.String())
}

// sourceURLTransport builds the hardened transport used by both the
// proxy-add-source fetch path and the proxy-paste URL fetch path. The dial
// guard rejects any already-resolved non-global destination; hostname→IP
// resolution-and-block for the initial host is handled implicitly because
// the transport dial receives the resolver's result (here we guard the
// literal IP), and redirect targets are guarded by ssrfCheckRedirect.
func sourceURLTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:       1,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	t.DialContext = ssrfDialContext
	return t
}
