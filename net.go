package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
	"golang.org/x/sync/singleflight"
)

type DialContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)
type DialTlsContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)

// DialPacketContextFunction opens a packet connection to addr. The extender
// packet dial returns one of these so quic can run over a carrier that is
// itself a reliable stream (net_extender_datagram.go).
type DialPacketContextFunction = func(ctx context.Context, network string, addr string) (net.PacketConn, error)

// dnsCacheEntry holds the resolved IPs plus their expiry time. ip is the first
// answer and the only one older entries carry; ips, when set, is the full set
// the caller picks a healthy address from.
type dnsCacheEntry struct {
	ip     string
	ips    []string
	expiry time.Time
}

func (e dnsCacheEntry) all() []string {
	if len(e.ips) > 0 {
		return e.ips
	}
	return []string{e.ip}
}

// dnsCache caches hostname-to-IPv4 lookups for proxy SOCKS5 CONNECT
// targets, so the system resolver isn't hit on every single dial through
// every proxy. TTL is 60 seconds — long enough to absorb burst dials from
// 2000+ concurrent warmup goroutines, short enough to pick up DNS changes.
//
// It backs v4 mode only. In proxy mode (proxy_target_family, see
// proxy_target_family.go) the destination name goes to the proxy and nothing
// here is consulted or written: there is no local address to cache, and a stale
// one would be a family choice the provider has no business making. Keeping the
// path intact for v4 is what lets the mode be flipped back with no other
// change.
var dnsCache struct {
	mu  sync.Mutex
	m   map[string]dnsCacheEntry
	sg  singleflight.Group
	neg map[string]time.Time // negative cache: host -> expiry
}

const dnsCacheTTL = 60 * time.Second

// dnsCacheMaxEntries bounds the CONNECT-target DNS cache. Keys are arbitrary
// client-requested hostnames (any host a proxy user asks to reach), so an
// untrusted or botted client population could otherwise grow this map without
// bound for the life of the process (finding #4). On insert overflow we evict
// expired entries first, then arbitrary ones to get back under the cap.
const dnsCacheMaxEntries = 4096

// proxyDNSResolveTimeout bounds the DoH resolution on each proxy dial so
// a slow/dead resolver can't block SOCKS5 CONNECT for longer than this.
// Shorter than RequestTimeout (15s) so the semaphore admission gate in
// DohCache.QueryResult can shed under load instead of backing up.
const proxyDNSResolveTimeout = 3 * time.Second

// lookupProxyTarget resolves a proxy target hostname to an IPv4 address.
// It is the v4 half of proxy_target_family (proxy_target_family.go) and the
// only caller of the cache below: proxy mode never reaches it, so nothing is
// resolved, cached, or held against a target's health there.
// When a shared DohCache has been registered (via SetSharedDohCache), DNS
// resolution goes through the cache rather than net.DefaultResolver — gaining
// serve-stale (RFC 8767) during resolver outages, server-scoring-based fan-out
// ordering, and lifecycle integration.
// ctx carries the per-dial context so the caller's deadline bounds the
// resolution independently of the DoH RequestTimeout.
func lookupProxyTarget(ctx context.Context, host string) (string, bool) {
	// Derive a bounded context for the DNS resolution so it can't stall
	// the proxy dial longer than proxyDNSResolveTimeout even when the
	// caller's ctx has a longer deadline (or none at all).
	resolveCtx, resolveCancel := context.WithTimeout(ctx, proxyDNSResolveTimeout)
	defer resolveCancel()

	// Try the shared DohCache first (if one is registered).
	sharedDohCacheMu.Lock()
	c := sharedDohCacheVal
	sharedDohCacheMu.Unlock()
	if c != nil {
		addrs := c.Query(resolveCtx, "A", host)
		if len(addrs) > 0 {
			ips := make([]string, len(addrs))
			for i, a := range addrs {
				ips[i] = a.String()
			}
			return chooseTargetAddr(ctx, host, ips), true
		}
		// Cache returned empty — fall through to the local resolver + stale
		// cache path below.
	}

	// Do NOT hold dnsCache.mu across the blocking LookupNetIP below: the
	// cache-miss path is a multi-hundred-ms network round-trip, and holding
	// the global lock across it would serialize every cache-miss proxy dial
	// behind one mutex (and pile prune work on top). Read under the lock,
	// release, resolve, then re-acquire to store.
	dnsCache.mu.Lock()
	if dnsCache.m == nil {
		dnsCache.m = make(map[string]dnsCacheEntry)
	}
	// Check negative cache first — avoid re-resolving hosts we recently
	// failed to resolve (finding #4).
	if dnsCache.neg != nil {
		if negExpiry, negOK := dnsCache.neg[host]; negOK && time.Now().Before(negExpiry) {
			dnsCache.mu.Unlock()
			return "", false
		}
	}
	e, ok := dnsCache.m[host]
	if ok && time.Now().Before(e.expiry) {
		ips := e.all()
		dnsCache.mu.Unlock()
		return chooseTargetAddr(ctx, host, ips), true
	}
	dnsCache.mu.Unlock()

	// Use singleflight to coalesce concurrent lookups for the same host.
	// Without this, 2000 warmup goroutines that all miss the cache each
	// issue their own LookupNetIP (finding #3 — resolver stampede).
	result, err, _ := dnsCache.sg.Do(host, func() (any, error) {
		ips, err := net.DefaultResolver.LookupNetIP(resolveCtx, "ip4", host)
		if err != nil {
			return nil, fmt.Errorf("dns: %w", err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("dns: no addresses for %s", host)
		}
		all := make([]string, len(ips))
		for i, a := range ips {
			all[i] = a.String()
		}
		return all, nil
	})

	if err != nil {
		// Lookup failed — check stale cache, then negative cache.
		dnsCache.mu.Lock()
		stale, staleOK := dnsCache.m[host]
		if staleOK && time.Now().Before(stale.expiry.Add(5*dnsCacheTTL)) {
			// Serve stale even past expiry, bounded to 5x TTL (RFC 8767).
			ips := stale.all()
			dnsCache.mu.Unlock()
			return chooseTargetAddr(ctx, host, ips), true
		}
		// Cache the negative result for 10s to avoid hammering the resolver
		// for the same nonexistent hostname (finding #4). Prune here too:
		// the success path is the only place pruneDNSCacheLocked runs today,
		// and a workload of distinct failing lookups never reaches it (M7).
		if dnsCache.neg == nil {
			dnsCache.neg = make(map[string]time.Time)
		}
		dnsCache.neg[host] = time.Now().Add(10 * time.Second)
		pruneDNSCacheNegLocked(time.Now())
		dnsCache.mu.Unlock()
		return "", false
	}

	ips := result.([]string)

	// Re-acquire to store the fresh result; re-check for a concurrent
	// writer's fresher entry so we don't clobber it.
	dnsCache.mu.Lock()
	if e2, ok2 := dnsCache.m[host]; ok2 && time.Now().Before(e2.expiry) {
		ips = e2.all()
		dnsCache.mu.Unlock()
		return chooseTargetAddr(ctx, host, ips), true
	}
	// Clear negative cache entry on success.
	if dnsCache.neg != nil {
		delete(dnsCache.neg, host)
	}
	dnsCache.m[host] = dnsCacheEntry{ip: ips[0], ips: ips, expiry: time.Now().Add(dnsCacheTTL)}
	// Bound the map against unbounded client-requested hostname growth (#4).
	// We hold dnsCache.mu here, so pruning under the same lock is safe and
	// does not contend with concurrent readers.
	pruneDNSCacheLocked(time.Now())
	dnsCache.mu.Unlock()
	return chooseTargetAddr(ctx, host, ips), true
}

// pruneDNSCacheLocked bounds the CONNECT-target DNS cache. Caller holds
// dnsCache.mu. Recovers if an entry is beyond the freshness window (what a
// periodic sweep would do) and, if the map is still over the cap, evicts
// expired entries then arbitrary ones to keep it bounded against unbounded
// client-requested hostname growth (finding #4).
// negPruneCap is the maximum size of the negative DNS cache.
const negPruneCap = 1024

// pruneDNSCacheNegLocked sweeps expired negative-cache entries and caps the
// map at negPruneCap by arbitrary eviction. Caller holds dnsCache.mu.
// Called both on the failure path (M7: a workload of distinct failing
// lookups never reaches the success path's pruneDNSCacheLocked) and from
// pruneDNSCacheLocked itself.
func pruneDNSCacheNegLocked(now time.Time) {
	if len(dnsCache.neg) == 0 {
		return
	}
	for k, exp := range dnsCache.neg {
		if now.After(exp) {
			delete(dnsCache.neg, k)
		}
	}
	for len(dnsCache.neg) > negPruneCap {
		for k := range dnsCache.neg {
			delete(dnsCache.neg, k)
			break
		}
	}
}

func pruneDNSCacheLocked(now time.Time) {
	// M7: the negative cache is swept on EVERY prune call, independent of
	// the positive-cache-size early returns below. pruneDNSCacheLocked only
	// runs on the success path, where a workload of distinct failing lookups
	// never trips the positive threshold — without this unconditional sweep,
	// dnsCache.neg would grow without bound.
	pruneDNSCacheNegLocked(now)

	const targetPercent = 90
	target := dnsCacheMaxEntries * targetPercent / 100

	if len(dnsCache.m) <= target {
		return
	}

	// Phase 1: drop expired entries first (cheap, free).
	for k, e := range dnsCache.m {
		if !now.Before(e.expiry) {
			delete(dnsCache.m, k)
		}
	}
	if len(dnsCache.m) <= target {
		return
	}

	// Phase 2: collect (key, expiry) pairs, partial-sort to find
	// nearest-to-expiry, then delete — O(n) total instead of O(n²).
	// NOTE: toEvict is computed from the post-Phase-1 map size. The pairs
	// snapshot below is a copy; deletions during Phase 2 iteration affect the
	// map but not the slice, so toEvict remains correct.
	toEvict := len(dnsCache.m) - target
	type kv struct {
		key    string
		expiry time.Time
	}
	pairs := make([]kv, 0, len(dnsCache.m))
	for k, e := range dnsCache.m {
		pairs = append(pairs, kv{k, e.expiry})
	}
	// Partial selection: find the toEvict nearest-to-expiry entries.
	// Simple insertion sort for small eviction counts is fine.
	for i := 0; i < toEvict && i < len(pairs); i++ {
		minIdx := i
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].expiry.Before(pairs[minIdx].expiry) {
				minIdx = j
			}
		}
		pairs[i], pairs[minIdx] = pairs[minIdx], pairs[i]
		delete(dnsCache.m, pairs[i].key)
	}
}

func DefaultConnectSettings() *ConnectSettings {
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		panic(err)
	}
	return &ConnectSettings{
		RequestTimeout:   15 * time.Second,
		ConnectTimeout:   15 * time.Second,
		TlsTimeout:       15 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		IdleConnTimeout:  90 * time.Second,
		KeepAliveTimeout: 5 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     5 * time.Second,
			Interval: 5 * time.Second,
			Count:    1,
		},

		// the floor a stalled first control handshake hits, and the room a
		// retry over the other family needs (see
		// dialControlTlsWithFamilyFallback)
		ControlFamilyFirstHandshakeTimeout: 8 * time.Second,
		ControlFamilyRetryReserve:          5 * time.Second,

		TlsConfig: tlsConfig,
		// Go's own hello. The Chrome parrot is opt-in until a dial soak
		// passes: the provider turns it on with the tls_hello control key or
		// URNETWORK_TLS_HELLO, through the runtime gate
		// (net_tls_hello_gate.go), never by a per-strategy default.
		TlsClientHelloFingerprint: TlsClientHelloFingerprintGo,

		// spell out the zero value's meaning: a version 2 Initial first,
		// version 1 behind it (net_quic_version.go)
		QuicVersionPolicy: QuicVersionPolicyPreferV2,
	}
}

type ConnectSettings struct {
	// Log, when set, is used by the connect functions. nil resolves to
	// DefaultLogger().
	Log Logger

	// The QUIC versions the platform h3 carriers offer (net_quic_version.go).
	// The zero value offers version 2 first with version 1 behind it.
	QuicVersionPolicy QuicVersionPolicy

	RequestTimeout   time.Duration
	ConnectTimeout   time.Duration
	TlsTimeout       time.Duration
	HandshakeTimeout time.Duration
	IdleConnTimeout  time.Duration
	KeepAliveTimeout time.Duration
	KeepAliveConfig  net.KeepAliveConfig

	// ControlFamilyFirstHandshakeTimeout bounds the FIRST tls handshake of a
	// control dial (dialControlTlsWithFamilyFallback) so that the retry over
	// the other address family has somewhere to run inside the caller's own
	// budget. It is what a stalled handshake hits instead of the caller's
	// deadline, and hitting it is what records a demotion.
	//
	// A FLOOR, not a fraction of the caller's budget. A fraction scales down
	// with the caller and turns a merely slow handshake into "this family is
	// blackholed" -- halving the platform control websocket's 5s gave the
	// first handshake 2.5s, which a congested mobile link reaches with a
	// pinned P-384 chain and nothing wrong. A fixed floor cannot do that: 8s
	// is more than the whole of `HandshakeTimeout`, and `HandshakeTimeout` is
	// the budget in which every shipping platform websocket dial already
	// completes a tcp connect, this same tls handshake AND an http upgrade.
	// A handshake past 8s is one this product already treats as failed
	// everywhere else.
	//
	// It is deliberately NOT `TlsTimeout`: at 15s it is at or above every
	// production caller's own budget, so nothing ever reaches it and the
	// retry never runs (see dialControlTlsWithFamilyFallback).
	//
	// <= 0 disables the bound; the first handshake then gets the caller's
	// whole remaining budget.
	ControlFamilyFirstHandshakeTimeout time.Duration

	// ControlFamilyRetryReserve is how much of the caller's budget must
	// remain AFTER a bounded first handshake before the bound is applied at
	// all. Below `ControlFamilyFirstHandshakeTimeout + ControlFamilyRetryReserve`
	// the first handshake is left unbounded, because a bound that produces a
	// timeout with no room to retry is strictly worse than no bound: it turns
	// a request that would have kept waiting into one that fails early.
	//
	// `HandshakeTimeout`'s 5s again, for the same reason: it is this
	// codebase's own statement of what a COMPLETE control dial needs, so a
	// retry given that much is given as much as a whole production websocket
	// dial gets.
	//
	// This is also what keeps the bound off the platform control websocket.
	// gorilla caps that dial context at `HandshakeTimeout`, and 5s is less
	// than 8s + 5s, so it is never bounded and its handshake tolerance is
	// untouched. It does not need its own retry: the demotion ledger is
	// process-global, so what the api path learns is already in force here.
	//
	// <= 0 disables the bound.
	ControlFamilyRetryReserve time.Duration

	TlsConfig *tls.Config

	// The tls client hello of the normal and resilient dialers
	// (net_tls_hello.go). `TlsClientHelloFingerprintChrome` asks for a current
	// Chrome hello through uTLS. `TlsClientHelloFingerprintGo`, empty, or any
	// other value keeps Go's own
	// crypto/tls hello: the kill switch, should the Chrome hello be refused
	// somewhere. `DefaultConnectSettings` sets Go's hello, so the parrot is
	// opt-in until a dial soak passes. Once set, the process-wide runtime gate
	// (`SetTlsClientHelloChrome`, net_tls_hello_gate.go) overrides this base
	// for every dial in both directions; the provider drives that gate from
	// its tls_hello control key and URNETWORK_TLS_HELLO. Either way a dial
	// keeps its server name, certificate verification, application protocols
	// and timeouts. The extender, VLESS and alt dialers, DoH and the streaming
	// post keep their own hellos.
	TlsClientHelloFingerprint string

	ProxySettings *ProxySettings
	Resolver      *net.Resolver

	DialContextSettings *DialContextSettings

	// DialControl, when set, runs on every socket the default dialer creates,
	// letting the caller bind egress or mark the socket (ported from upstream
	// net.go at 20b29f79; nil is a no-op).
	DialControl func(network string, address string, c syscall.RawConn) error
	// DialNetworkHook, when set, is called at the top of DialContext with the
	// network and address before any narrowing (ported from upstream; nil is a
	// no-op).
	DialNetworkHook func(network string, addr string)

	DisableIpv4 bool
	DisableIpv6 bool
}

// dialUsesProxy reports whether DialContext will be the SOCKS path, in which
// case a completed connection's remote address describes the hop to the proxy,
// not the destination or the host's own route to it.
func (self *ConnectSettings) dialUsesProxy() bool {
	return self.DialContextSettings == nil && self.ProxySettings != nil
}

type DialContextSettings struct {
	DialContext DialContextFunction
	// Only Tun sets this diagnostic marker; other supplied dialers stay caller.
	dohTun bool
	// PacketConnFactory, when set, supplies the packet conn for udp dials so a
	// pinned transport can bind its own family (ported from upstream at
	// 20b29f79, inert here; nil keeps the existing behavior).
	PacketConnFactory func(context.Context) (net.PacketConn, error)
}

// DialContext dials network/addr, applying the family policy from
// DisableIpv4/DisableIpv6: explicitly requested families that are disabled
// (tcp4/tcp6/udp4/udp6) are rejected, while generic "tcp"/"udp" are remapped
// to the enabled family.
//
// It then applies the CONTROL-plane family policy (control_family.go): a
// developer Force narrows the same way, and, in Auto, a LEARNED demotion --
// written by dialControlTlsWithFamilyFallback after a connect succeeded and
// the handshake then timed out -- narrows every control dial in the process
// onto the other family. A dial whose addr is an IP literal is left alone in
// every case: there is no resolution step left to steer, so narrowing could
// only turn a working dial into "no suitable address found".
//
// With no force set and no demotion learned, controlDialNetwork is the
// identity, so this is exactly the dial this tree made before the policy
// existed. The dial goes through DialContextSettings.DialContext
// when set, otherwise through ProxySettings.NewDialContext when a proxy is
// configured, otherwise through a plain NetDialer. Dial results are logged
// at V(2).
func (self *ConnectSettings) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	if hook := self.DialNetworkHook; hook != nil {
		hook(network, addr)
	}
	if self.DisableIpv4 && self.DisableIpv6 {
		return nil, fmt.Errorf("ipv4 and ipv6 are both disabled")
	}
	switch network {
	case "tcp":
		if self.DisableIpv4 {
			network = "tcp6"
		} else if self.DisableIpv6 {
			network = "tcp4"
		}
	case "tcp4":
		if self.DisableIpv4 {
			return nil, fmt.Errorf("ipv4 is disabled")
		}
	case "tcp6":
		if self.DisableIpv6 {
			return nil, fmt.Errorf("ipv6 is disabled")
		}
	case "udp":
		if self.DisableIpv4 {
			network = "udp6"
		} else if self.DisableIpv6 {
			network = "udp4"
		}
	case "udp4":
		if self.DisableIpv4 {
			return nil, fmt.Errorf("ipv4 is disabled")
		}
	case "udp6":
		if self.DisableIpv6 {
			return nil, fmt.Errorf("ipv6 is disabled")
		}
	}

	network, networkErr := controlDialNetwork(network, addr)
	if networkErr != nil {
		return nil, networkErr
	}

	var dialContext DialContextFunction

	if self.DialContextSettings != nil {
		dialContext = self.DialContextSettings.DialContext
	} else {
		netDialer := self.NetDialer()
		if self.ProxySettings != nil {
			dialContext = self.ProxySettings.NewDialContext(
				ctx,
				netDialer,
			)
		} else {
			dialContext = netDialer.DialContext
		}
	}

	conn, err := dialContext(ctx, network, addr)
	if log := loggerOrDefault(self.Log).V(2); log.Enabled() {
		// the address is printed as it was dialed and the error as it came
		// back, so a failed proxied dial says which proxy it reached; only a
		// credential in the error text is removed
		if err == nil {
			log.Infof("[net]dial %s %s success\n", network, addr)
		} else {
			log.Infof("[net]dial %s %s err=%s\n", network, addr, errText(err))
		}
	}
	return conn, err
}

// NetDialer builds a fresh net.Dialer from the settings' connect timeout,
// keep-alive configuration, and resolver; each call returns an independent
// dialer, so per-dial state never shares between calls.
func (self *ConnectSettings) NetDialer() *net.Dialer {
	// providerEgressDialer is inert unless the dial context carries a provider
	// egress target (see providerEgressTarget.dialContext).
	return providerEgressDialer(&net.Dialer{
		Timeout:         self.ConnectTimeout,
		KeepAlive:       self.KeepAliveTimeout,
		KeepAliveConfig: self.KeepAliveConfig,
		Control:         self.DialControl,
		Resolver:        self.Resolver,
	})
}

type ProxySettings struct {
	Index   int
	Network string
	Address string
	Auth    *proxy.Auth
}

// RedactProxyCredentials removes the userinfo a proxy address in text carries:
// the "user:password@" in front of the host, with or without a URI scheme, and
// a user name alone when it sits in front of an address. It is the only
// transformation a line about a proxy needs. The address, the hop it resolved
// to, the dial error around it and the words in front of it are copied through
// unchanged, so a log line still says which proxy it is about and a dial error
// still carries the address Go printed.
func RedactProxyCredentials(text string) string {
	return redactProxyCredentials(text, nil)
}

// errText is err's message with any proxy credential removed, for a log line.
// It is nil safe so a site that logs an error which may be nil needs no branch.
func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return RedactProxyCredentials(err.Error())
}

// proxyRedactStats counts the work one pass did, so a test can assert that the
// cost stays linear in the length of the line instead of timing it.
type proxyRedactStats struct {
	// scanned is the number of bytes the backward credential scans walked.
	scanned int
}

// add records n scanned bytes; a nil stats is the production path.
func (self *proxyRedactStats) add(n int) {
	if self != nil {
		self.scanned += n
	}
}

func redactProxyCredentials(text string, stats *proxyRedactStats) string {
	if !strings.Contains(text, "@") {
		return text
	}
	out := text
	search := 0
	for {
		rel := strings.IndexByte(out[search:], '@')
		if rel < 0 {
			return out
		}
		at := search + rel
		// the text after the '@' decides whether the run in front of it is
		// userinfo: only an address a proxy could be dialed at has any
		host, port, ok := proxyTokenAfter(out, at+1)
		if !ok || !proxyTokenNamesAddress(host, port) {
			search = at + 1
			continue
		}
		start := proxyAuthorityStart(out, at, stats)
		if at <= start {
			search = at + 1
			continue
		}
		out = out[:start] + out[at+1:]
		search = start
	}
}

// proxyTokenNamesAddress reports whether the token after an '@' is an address a
// proxy is dialed at, which is what makes the run in front of it a credential.
// A name needs its port, and a token with no port at all counts only when it is
// a numeric address, so a bare "@name" in prose stays as it is.
func proxyTokenNamesAddress(host string, port string) bool {
	if !ProxyHostAllowed(host) {
		return false
	}
	if port == "" {
		return proxyIsAddr(host)
	}
	return ProxyPortAllowed(port)
}

// proxyCredentialScanLimit bounds one backward credential scan, in bytes. A
// credential is a userinfo run, so the bound only matters for text that is not
// one: a long run of credential bytes in front of an '@' would otherwise be
// walked once per at-sign and make the pass quadratic in the length of the line.
// Text over the bound keeps the bytes in front of it, which is at most the run
// minus the bound, and loses the rest.
const proxyCredentialScanLimit = 1024

// proxyTokenAfter is the host and port of the address token starting at start,
// and whether one starts there at all. An error prints the address it failed on
// followed by a colon ("dial host:port: connection refused"), and that colon is
// part of the token but not of the address, so it is dropped before the host is
// read.
func proxyTokenAfter(text string, start int) (string, string, bool) {
	if len(text) <= start || (text[start] != '[' && !addressTokenByte(text[start])) {
		return "", "", false
	}
	token := text[start:readAddressToken(text, start)]
	if head, ok := strings.CutSuffix(token, ":"); ok && addressHeadIsHostPort(head) {
		token = head
	}
	host, port := splitAddressToken(token)
	return host, port, true
}

// addressHeadIsHostPort reports whether head, a token that ended with the colon
// an error prints after an address, is that address without the colon. A bare
// v6 literal that ends in ':' is left alone, since the colon there is part of
// the address.
func addressHeadIsHostPort(head string) bool {
	if host, port, err := net.SplitHostPort(head); err == nil {
		return host != "" && port != ""
	}
	return strings.HasPrefix(head, "[") && strings.HasSuffix(head, "]")
}

// addressTokenByte reports whether b can belong to a host or a port. A closing
// bracket is not one of them: a v6 literal is read whole by readAddressToken,
// and past that literal a ']' is the bracket a log line wrapped the value in,
// which ends the token rather than belonging to the port behind it.
func addressTokenByte(b byte) bool {
	switch {
	case '0' <= b && b <= '9', 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return true
	case b == '.', b == ':', b == '-', b == '_', b == '%':
		return true
	}
	return false
}

// readAddressToken is the end of the address token starting at start. A
// bracketed v6 literal is read as one token with the port that follows it. The
// hyphen of the "->" an OpError prints between two addresses ends the token, so
// the port in front of it is read whole.
func readAddressToken(text string, start int) int {
	i := start
	if text[i] == '[' {
		closing := strings.IndexByte(text[i:], ']')
		if closing < 0 {
			return len(text)
		}
		i += closing + 1
	}
	for i < len(text) && addressTokenByte(text[i]) {
		// the hyphen of the "->" between two printed addresses is a separator,
		// not part of a host or a port; a token that would otherwise be empty
		// takes it, so the caller always moves forward
		if text[i] == '-' && i+1 < len(text) && text[i+1] == '>' {
			if i == start {
				i++
			}
			break
		}
		i++
	}
	return i
}

// splitAddressToken separates a token into host and port. The port is empty for
// a bare host, and a bare v6 literal is read whole.
func splitAddressToken(token string) (string, string) {
	if strings.HasPrefix(token, "[") {
		closing := strings.IndexByte(token, ']')
		if closing < 0 {
			return "", ""
		}
		return token[1:closing], strings.TrimPrefix(token[closing+1:], ":")
	}
	if host, port, err := net.SplitHostPort(token); err == nil {
		return host, port
	}
	return token, ""
}

// proxyAuthorityStart is where the credential in front of the '@' at end begins.
// The token the credential sits in is read first. '=' is both a sub-delimiter a
// password may carry and the separator a log line puts between a key and its
// value ("err=", "addr="): it separates a key when the bytes between it and the
// token start are a plain identifier, so "err=demo:pw@host" keeps "err=", and
// "demo:p=ss@host" loses "demo:p=ss". A bare "user=pw@host" is read as a key,
// because an unquoted line of that shape is far more often a key than a
// credential.
//
// A token that opens with a URI scheme starts the credential after the scheme
// instead. The scheme fixes the start of that credential and the '@' its end, so
// the punctuation that bounds a scheme-less one - ',', ';', the brackets, a
// pipe, a quote, a backslash - is part of the password and goes with it. Only
// the outermost scheme belongs to the URL: a password may carry a scheme of its
// own ("demo:p=https://tail@host"), and a proxy URL with that shape is one an
// operator ends up with, so a later "://" is part of the credential and never a
// new URL to start from.
func proxyAuthorityStart(text string, end int, stats *proxyRedactStats) int {
	if start := proxySchemeTokenStart(text, end, stats); 0 <= start {
		return start
	}
	return proxyCredentialStart(text, end, stats)
}

// proxySchemeTokenStart is where the credential begins when the token holding it
// opens with a URI scheme, and -1 when it does not. The scan runs back over the
// punctuation a password may carry in that form, stops at whitespace and at a
// key's '=', then skips the quote or bracket a log line puts in front of the
// value. The token counts only when a run of scheme bytes opens it and the
// bytes after that run are "://".
func proxySchemeTokenStart(text string, end int, stats *proxyRedactStats) int {
	start := end
	for limit := max(end-proxyCredentialScanLimit, 0); limit < start; {
		if text[start-1] == '=' && proxyEqualsIsKey(text, start-1) {
			break
		}
		if !proxySchemeCredentialByte(text[start-1]) {
			break
		}
		start--
	}
	stats.add(end - start)
	for start < end && proxyTokenOpenByte(text[start]) {
		start++
	}
	run := start
	for run < end && proxySchemeByte(text[run]) {
		run++
	}
	stats.add(run - start)
	if start < run && !('0' <= text[start] && text[start] <= '9') && strings.HasPrefix(text[run:end], "://") {
		return run + 3
	}
	return -1
}

// proxySchemeCredentialByte reports whether b can belong to the credential of a
// token that opens with a URI scheme: the userinfo set, plus the sub-delimiters
// and punctuation a password may carry there, since the scheme and the '@' fix
// both ends of the run and nothing in it has to bound the prose around it.
func proxySchemeCredentialByte(b byte) bool {
	if proxyCredentialByte(b) {
		return true
	}
	switch b {
	case ',', ';', '(', ')', '<', '>', '[', ']', '{', '}', '|', '"', '\'', '`', '\\':
		return true
	}
	return false
}

// proxyTokenOpenByte reports whether b is a character a log line puts in front
// of a value rather than part of it.
func proxyTokenOpenByte(b byte) bool {
	switch b {
	case '"', '\'', '`', '(', '[', '{', '<':
		return true
	}
	return false
}

// proxyCredentialStart is the first byte of the token the userinfo in front of
// end sits in, with the '=' of a key left in front of it. Whitespace, a quote
// and the brackets and punctuation a log line puts around a value end the token
// there, so a password carrying one of those loses only the part behind it
// instead of taking the surrounding prose with it.
func proxyCredentialStart(text string, end int, stats *proxyRedactStats) int {
	start := end
	for limit := max(end-proxyCredentialScanLimit, 0); limit < start; {
		if text[start-1] == '=' && proxyEqualsIsKey(text, start-1) {
			break
		}
		if !proxyCredentialByte(text[start-1]) {
			break
		}
		start--
	}
	stats.add(end - start)
	return start
}

// proxySchemeByte reports whether b can belong to a URI scheme.
func proxySchemeByte(b byte) bool {
	switch {
	case '0' <= b && b <= '9', 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return true
	case b == '+', b == '-', b == '.':
		return true
	}
	return false
}

// proxyEqualsIsKey reports whether the '=' at index i separates a key from its
// value rather than sitting inside userinfo. A key is a plain identifier at the
// start of the token.
func proxyEqualsIsKey(text string, i int) bool {
	start := i
	for 0 < start && proxyKeyByte(text[start-1]) {
		start--
	}
	if start == i {
		return false
	}
	return start == 0 || proxyTokenBoundaryByte(text[start-1])
}

// proxyKeyByte reports whether b can belong to a plain key such as "err".
func proxyKeyByte(b byte) bool {
	switch {
	case '0' <= b && b <= '9', 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return true
	case b == '_', b == '-', b == '.':
		return true
	}
	return false
}

// proxyTokenBoundaryByte reports whether b ends the token a credential sits in:
// whitespace, a quote, and the brackets and punctuation a log line puts around
// a value. A password may carry ',' or ';', but taking those back would swallow
// the prose around a line, so the boundary wins.
func proxyTokenBoundaryByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	case '"', '\'', '`', '(', ')', '<', '>', '[', ']', '{', '}', '|', ',', ';':
		return true
	}
	return false
}

// proxyCredentialByte reports whether b can belong to userinfo. It includes the
// '@' a password may carry and the sub-delimiters "& = ? #" as well as the
// unreserved and percent-encoded forms, so the run is taken from its true
// start.
func proxyCredentialByte(b byte) bool {
	switch {
	case '0' <= b && b <= '9', 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return true
	case b == '-', b == '.', b == '_', b == '~', b == '%', b == ':', b == '!',
		b == '$', b == '&', b == '*', b == '+', b == '/', b == '=', b == '?',
		b == '#', b == '@':
		return true
	}
	return false
}

// proxyIsAddr reports whether host is a numeric address rather than a name.
func proxyIsAddr(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// String omits credentials and rejects addresses that are not bare host:port.
func (self ProxySettings) String() string {
	return fmt.Sprintf("proxy[%d] (%s)", self.Index, safeProxyDisplayAddress(self.Address))
}

// GoString protects Go-syntax formatting as well as ordinary formatting.
func (self ProxySettings) GoString() string { return self.String() }

// proxyKeyUserSep separates Address from Auth.User in Key(). A proxy
// provider that shares one gateway host:port across multiple accounts (the
// account, not the address, decides which backend IP you land on) is
// identified by address+user, not address alone. \x1f is used because it
// cannot occur in a host:port and is exceedingly unlikely in a username, and
// it keeps an unauthenticated proxy's key byte-identical to its bare
// Address — the load-bearing property that lets every persisted store
// (proxy.state, earnings) keep reading old data with no migration for the
// unauthenticated case, and a bounded one for the authenticated case.
const proxyKeyUserSep = "\x1f"

// Key returns this proxy's unique identity: Address alone when there is no
// auth, or Address+proxyKeyUserSep+Auth.User when there is. Password is
// deliberately excluded — a credential rotation (same account, new
// password) must stay the same identity so its ID, health history, and
// earnings survive the rotation, exactly as sameAuth's existing rotation
// semantics already assume. Two proxies that differ only by password are a
// rotation, not a new proxy; two proxies at the same Address with different
// Auth.User are genuinely different proxies.
func (self *ProxySettings) Key() string {
	if self.Auth == nil || self.Auth.User == "" {
		return self.Address
	}
	return self.Address + proxyKeyUserSep + self.Auth.User
}

// SplitProxyKey is Key()'s inverse: it separates a key back into its
// address and (possibly empty) user, for callers that only have the key —
// e.g. formatting an operator-facing log line, where the raw key's \x1f
// separator must never appear verbatim.
func SplitProxyKey(key string) (address, user string) {
	address, user, ok := strings.Cut(key, proxyKeyUserSep)
	if !ok {
		return key, ""
	}
	return address, user
}

// proxyDialError marks a failure that happened on a proxied dial. It is
// returned by ProxySettings.NewDialContext and exists for exactly one reader:
// recordControlFamilyStrike (control_family.go).
//
// The distinction it preserves is the difference between a bug and a six-hour
// outage. A proxied control dial connects to the proxy first, so the family of
// that socket is the PROXY's family, not the destination's, and a stall or a
// failure after the CONNECT is the proxy or its upstream, not this host's
// route. Striking IPv4 for it demotes the family every other control dial in
// the process depends on, which is what happened on NY5: a fleet of proxies
// that could not reach the platform read as "IPv4 is blackholed" and pinned the
// whole node onto an IPv6 path that was not there.
//
// Error() is the underlying message VERBATIM, with no prefix, because the
// connect classifiers in reconnect_governor.go and proxy_health.go read error
// strings: a marker that changed one of those strings would change proxy health
// and reconnect accounting, which is a bigger blast radius than the bug this
// guards. Unwrap keeps errors.Is and errors.As working through the marker.
type proxyDialError struct {
	index int
	err   error
}

func (self *proxyDialError) Error() string { return self.err.Error() }

func (self *proxyDialError) Unwrap() error { return self.err }

// Timeout and Temporary forward the wrapped error's classification. They are
// here because a caller that type-asserts the concrete error to net.Error
// (rather than using errors.As) must still see a timed-out proxied dial as a
// timeout; without them the marker would silently change retry and backoff
// decisions on the proxied paths. A wrapped error that is not a net.Error
// reports false from both, which is the same answer net.Error gives a
// non-timeout failure.
func (self *proxyDialError) Timeout() bool {
	var netErr net.Error
	return errors.As(self.err, &netErr) && netErr.Timeout()
}

func (self *proxyDialError) Temporary() bool {
	var netErr net.Error
	return errors.As(self.err, &netErr) && netErr.Temporary()
}

// proxyIndex is the ProxySettings.Index of the proxy the dial went through, for
// a log line.
func (self *proxyDialError) proxyIndex() int { return self.index }

// proxySOCKS5DialTimeout is the maximum time a single SOCKS5 dial (TCP
// connect + greeting + CONNECT) is allowed to take through a proxy. Paths
// that already carry a context deadline (e.g. serialEval with its 15s
// RequestTimeout) are unaffected — this only applies when the caller's
// context has no deadline, which happens during the startup warmup burst
// where 2000+ goroutines compete for admission and can pile up
// indefinitely.
const proxySOCKS5DialTimeout = 30 * time.Second

// NewDialContext returns a DialContextFunction that dials through this
// SOCKS5 proxy using forward as the transport. What happens to a non-IP host
// depends on proxy_target_family (proxy_target_family.go): in v4 mode, the
// default, it is resolved through the package-level dnsCache and the CONNECT
// carries an IPv4 literal, exactly as before; in proxy mode the name goes to
// the proxy untouched so the proxy's resolver picks the family. When the
// caller's context carries no deadline the proxy dial is bounded by
// proxySOCKS5DialTimeout; successful connections are wrapped in a trackedConn
// that counts bytes against this proxy's bandwidth index. A failed dial is
// returned as a proxyDialError so family accounting can never read it as
// evidence about this host's address family (recordControlFamilyStrike in
// control_family.go).
func (self *ProxySettings) NewDialContext(ctx context.Context, forward proxy.Dialer) DialContextFunction {
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		// one dial of the connect attempt that carries these stats, if any
		attempt := attemptStatsFrom(ctx)
		if attempt != nil {
			attempt.dials.Add(1)
		}
		host, port, err := net.SplitHostPort(addr)
		targetHost := ""
		// only a dial that resolved the target here has an address of its own
		// to record an outcome against; a name handed to the proxy is the
		// proxy's resolution, not evidence this host can use that target
		targetUsesLocalAddress := false
		if err == nil {
			if ip := net.ParseIP(host); ip == nil {
				if proxyDialCarriesName(ctx) {
					// proxy mode: the CONNECT carries the hostname and the proxy
					// resolves it, so its own A and AAAA answers decide the
					// family. No local lookup and no dnsCache entry, which is
					// the point: there is no address here to choose, cache, or
					// hold against a target's health.
					targetHost = host
				} else if resolved, ok := lookupProxyTarget(ctx, host); ok {
					addr = net.JoinHostPort(resolved, port)
					targetHost = host
					targetUsesLocalAddress = true
				}
			}
		}

		proxyDialer, err := proxy.SOCKS5(
			self.Network,
			self.Address,
			self.Auth,
			forward,
		)
		if err != nil {
			return nil, &proxyDialError{index: self.Index, err: err}
		}

		dialCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			dialCtx, cancel = context.WithTimeout(ctx, proxySOCKS5DialTimeout)
			defer cancel()
		}

		// per-carrier v6 memory (net_carrier_ipv6.go): a dial that names v6
		// through a carrier whose v6 keeps failing is refused at once, so the
		// dial race falls to v4 instead of waiting out the timeout
		dialFamily := dialNamesFamily(network, addr)
		carrierKey := ""
		carrierProbe := false
		if dialFamily == 6 {
			carrierKey = self.Key()
			var allowed bool
			allowed, carrierProbe = carrierIpv6.allow(carrierKey)
			if !allowed {
				// Deliberately not wrapped in proxyDialError. That type tells
				// recordControlFamilyStrike the failure belonged to a proxy, and
				// this error needs no such protection: it is not a path timeout,
				// which is the only shape that strikes a family. Wrapping it would
				// only log a strike refusal on every suppressed dial.
				return nil, errCarrierIpv6Suppressed
			}
		}

		var conn net.Conn
		if v, ok := proxyDialer.(proxy.ContextDialer); ok {
			conn, err = v.DialContext(dialCtx, network, addr)
		} else {
			conn, err = proxyDialer.Dial(network, addr)
		}
		if dialFamily == 6 {
			carrierIpv6.done(carrierKey, carrierProbe, err, err != nil && carrierIpv6Neutral(err))
		} else if dialFamily == 4 && err != nil && !carrierIpv6Neutral(err) {
			carrierIpv6.noteV4Failure(self.Key())
		}
		if err != nil {
			// Every failure below this point happened to a SOCKS5 hop, which is
			// not this host's path: the family of the connection to the proxy
			// says nothing about the family of the destination, and neither does
			// the proxy's own upstream failure (see recordControlFamilyStrike).
			return nil, &proxyDialError{index: self.Index, err: err}
		}
		if attempt != nil && targetUsesLocalAddress {
			// the proxy reached the address this dial resolved: a stall from
			// here on is about that target. In proxy mode there is no such
			// address, so nothing is recorded until a dial resolves one.
			attempt.noteDialedTarget(targetHost, self.Index)
		}

		bw := RegisteredProxyBandwidth(self.Index)
		if bw != nil || attempt != nil {
			tc := &trackedConn{Conn: conn, bw: bw, attempt: attempt, v6: addrIsV6(conn.RemoteAddr())}
			return tc, nil
		}

		return conn, nil
	}
}

// trackDirect credits a socket's bytes to bw, the way the proxy dialers already
// do for theirs. A conn that is already tracked (a proxy dial returns one) is
// returned as is, so a byte is never counted twice, and a nil bw leaves the conn
// alone.
func trackDirect(conn net.Conn, bw *ProxyBandwidth) net.Conn {
	if bw == nil {
		return conn
	}
	if _, ok := conn.(*trackedConn); ok {
		return conn
	}
	return &trackedConn{Conn: conn, bw: bw, v6: addrIsV6(conn.RemoteAddr())}
}

type trackedConn struct {
	net.Conn
	bw *ProxyBandwidth
	// attempt, when set, also credits the bytes to the connect attempt that
	// opened this connection (see attemptStats)
	attempt *attemptStats
	// v6 is the address family of the connection, read once from its remote
	// address when the connection is wrapped. The family of a connection never
	// changes, so the byte counters pay one field test and no address parse.
	v6   bool
	once sync.Once
}

// addrIsV6 reports whether addr names an IPv6 endpoint. Anything unparseable
// counts as IPv4: a family the code cannot prove is never credited to IPv6.
func addrIsV6(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

// Read reads from the underlying connection and, when bw is set, credits the
// byte count to the proxy's TotalRx, and to TotalRxV6 when the connection's
// remote address is IPv6.
func (self *trackedConn) Read(b []byte) (n int, err error) {
	n, err = self.Conn.Read(b)
	if n > 0 {
		recordSocketBytes(self.socketPhase(), false, n)
	}
	if n > 0 && self.bw != nil {
		self.bw.TotalRx.Add(uint64(n))
		if self.v6 {
			self.bw.TotalRxV6.Add(uint64(n))
		}
	}
	if n > 0 && self.attempt != nil {
		self.attempt.rx.Add(int64(n))
	}
	return
}

// Write writes to the underlying connection and, when bw is set, credits the
// byte count to the proxy's TotalTx, and to TotalTxV6 when the connection's
// remote address is IPv6.
func (self *trackedConn) Write(b []byte) (n int, err error) {
	n, err = self.Conn.Write(b)
	if n > 0 {
		recordSocketBytes(self.socketPhase(), true, n)
	}
	if n > 0 && self.bw != nil {
		self.bw.TotalTx.Add(uint64(n))
		if self.v6 {
			self.bw.TotalTxV6.Add(uint64(n))
		}
	}
	if n > 0 && self.attempt != nil {
		self.attempt.tx.Add(int64(n))
	}
	return
}

// Close closes the underlying connection and returns its error. The once
// field's closure is empty, so Close still forwards every call — it does not
// make the close idempotent or adjust the bandwidth accounting.
func (self *trackedConn) Close() error {
	self.once.Do(func() {})
	return self.Conn.Close()
}
