package connect

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/idna"

	"golang.org/x/net/http2"
)

// dohFailureCount tracks DoH failures for aggregate reporting in health heartbeat
var dohFailureCount atomic.Int64

func GetDohFailureCount() uint64 {
	return uint64(dohFailureCount.Swap(0))
}

var dohErrThrottle = newLogThrottle(5 * time.Minute)

func shouldLogDohErr() (bool, int64) { return dohErrThrottle.Allow(time.Now()) }

func DefaultDohSettings() *DohSettings {
	return &DohSettings{
		ConnectSettings:          *DefaultConnectSettings(),
		IpVersion:                4,
		MissExpiration:           300 * time.Second,
		LocalExpiration:          300 * time.Second,
		MinCacheTtl:              30 * time.Second,
		CacheMaxEntries:          4096,
		MaxConcurrentResolutions: 64,
		// staggered hedge: a slow/dead primary fires one redundant server per
		// 750ms wave instead of all at once; a winning answer stops the rest.
		DohServerStagger:    750 * time.Millisecond,
		DnsResolverSettings: DefaultDnsResolverSettings(),
	}
}

// the resolver tries the following sequence until there is a found record:
// 1. if enable remote doh, remote doh
// 2. if enable local doh, local doh (host-dialed, e.g. a sidecar resolver)
// 3. if enable remote dns, remote dns
// 4. if enable local dns, local dns
//
// the remote doh servers are queried in parallel and the first server to return records wins
// (see dohQueryWithClientResult). each server is tagged with the format it speaks: Cloudflare and
// Google use the JSON API (application/dns-json, ?name=&type=); Quad9 and OpenDNS speak RFC 8484
// wire-format on :443 — both are supported. each must present an IP-SAN cert (these do). Quad9's
// JSON :5053 endpoint is retired / port-blocked, so it is queried as wire.
// https://developers.google.com/speed/public-dns/docs/doh/json
func DefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh: true,
		EnableLocalDns:  true,
		RemoteDohServersIpv4: []DohServer{
			{Url: "https://1.1.1.1/dns-query", Format: DohFormatJson},        // Cloudflare
			{Url: "https://8.8.8.8/resolve", Format: DohFormatJson},          // Google
			{Url: "https://9.9.9.9/dns-query", Format: DohFormatWire},        // Quad9 (RFC 8484)
			{Url: "https://208.67.222.222/dns-query", Format: DohFormatWire}, // OpenDNS (RFC 8484)
		},
		// local plain-dns servers: host-side resolution, and the tunnel resolver
		// when the local-dns toggle is enabled. Quad9 (9.9.9.9) leads so the OS does
		// not auto-upgrade the tunnel resolver to encrypted DNS (which would bypass
		// the UpgradeMux); see the sdk's defaultTunnelDnsServersIpv4
		LocalDnsIpv4: []string{
			"9.9.9.9", // Quad9
			"1.1.1.1", // Cloudflare
		},
	}
}

type DohSettings struct {
	ConnectSettings
	IpVersion       int
	MissExpiration  time.Duration
	LocalExpiration time.Duration
	// MinCacheTtl floors the cache lifetime of a resolved record so very-low / zero-TTL records
	// don't re-resolve (a full fan-out) on nearly every query. 0 disables the floor.
	MinCacheTtl     time.Duration
	CacheMaxEntries int
	// MaxConcurrentResolutions bounds in-flight resolutions (DohCache.resolveSem) so a burst
	// or flood of distinct names cannot fan out unbounded. 0 uses a sane default.
	MaxConcurrentResolutions int
	// DohServerStagger delays launching each additional DoH server within a fan-out: the first
	// server is queried immediately and each next one only if no answer has arrived within this
	// interval, so a healthy primary answers before the redundant servers fire (saving tunnel
	// bandwidth and parallel streams on the shared h2 connection). 0 fans out to all servers at
	// once (the old behavior). A winning server's answer stops further launches regardless.
	DohServerStagger time.Duration
	// ServerStatsSeed, when set, pre-loads the per-server success scores (url -> score, clamped
	// to dohSeedMaxScore) at construction so the weighted fan-out order starts from the last
	// session's experience instead of uniform-random. The cache's scores() exports the live
	// view for the owner to persist and pass back here on restart.
	ServerStatsSeed map[string]float64
	// MemoryTarget, when set, bounds resolution memory under load: each in-flight
	// DoH query reserves dohQueryReserveByteCount from it and releases on return.
	// nil = unbounded (the default; safe for typical provider memory).
	MemoryTarget        MemoryTarget
	DnsResolverSettings *DnsResolverSettings
}

// ResolverIp returns the network family string to pass to
// net.Resolver.LookupIP: "ip4" for IpVersion 4, "ip6" for IpVersion 6, and
// "ip" (both families) for any other value.
func (self *DohSettings) ResolverIp() string {
	switch self.IpVersion {
	case 4:
		return "ip4"
	case 6:
		return "ip6"
	default:
		return "ip"
	}
}

// DohFormat is the query encoding a DoH server speaks; the client picks the request format per
// server (see dohQueryWithClientResult).
type DohFormat string

const (
	// DohFormatJson is the Google-style JSON API (GET ?name=&type=, Accept application/dns-json).
	// Cloudflare and Google support it; it is the default for an empty format and for the legacy
	// RemoteDohUrls/LocalDohUrls fields.
	DohFormatJson DohFormat = "json"
	// DohFormatWire is RFC 8484 (GET ?dns=<base64url DNS message>, Accept application/dns-message).
	// Universally supported (Cloudflare, Google, Quad9, OpenDNS, ...).
	DohFormatWire DohFormat = "wire"
)

// DohServer is a DoH endpoint tagged with the query format it speaks.
type DohServer struct {
	Url    string    `json:"url"`
	Format DohFormat `json:"format,omitempty"` // empty == DohFormatJson
}

type DnsResolverSettings struct {
	EnableRemoteDoh   bool     `json:"enable_remote_doh,omitempty"`
	EnableLocalDoh    bool     `json:"enable_local_doh,omitempty"`
	EnableRemoteDns   bool     `json:"enable_remote_dns,omitempty"`
	EnableLocalDns    bool     `json:"enable_local_dns,omitempty"`
	RemoteDohUrlsIpv4 []string `json:"remote_doh_urls_ipv4,omitempty"`
	RemoteDohUrlsIpv6 []string `json:"remote_doh_urls_ipv6,omitempty"`
	LocalDohUrlsIpv4  []string `json:"local_doh_urls_ipv4,omitempty"`
	LocalDohUrlsIpv6  []string `json:"local_doh_urls_ipv6,omitempty"`
	// the *DohServers fields carry a per-server format tag (json or RFC 8484 wire). the legacy
	// *DohUrls []string fields above are still honored and treated as json; prefer these for new
	// config and for any wire-only server (e.g. Quad9, OpenDNS).
	RemoteDohServersIpv4 []DohServer `json:"remote_doh_servers_ipv4,omitempty"`
	RemoteDohServersIpv6 []DohServer `json:"remote_doh_servers_ipv6,omitempty"`
	LocalDohServersIpv4  []DohServer `json:"local_doh_servers_ipv4,omitempty"`
	LocalDohServersIpv6  []DohServer `json:"local_doh_servers_ipv6,omitempty"`
	RemoteDnsIpv4        []string    `json:"remote_dns_ipv4,omitempty"`
	RemoteDnsIpv6        []string    `json:"remote_dns_ipv6,omitempty"`
	LocalDnsIpv4         []string    `json:"local_dns_ipv4,omitempty"`
	LocalDnsIpv6         []string    `json:"local_dns_ipv6,omitempty"`

	// TlsConfig, if set, is used by the DoH HTTP clients — production cert pinning,
	// or trusting a local server's cert in tests. Not serialized.
	TlsConfig *tls.Config `json:"-"`
}

func httpClientWithSettings(settings *DohSettings) *http.Client {
	return httpClientWithDialer(settings, settings.DialContext)
}

// httpClientWithDialer builds a DoH HTTP client over the given dialer. Remote DoH
// uses the tun dialer (settings.DialContext); local DoH uses the host dialer.
func httpClientWithDialer(settings *DohSettings, dialContext DialContextFunction) *http.Client {
	tr := &http.Transport{
		DialContext:         dialContext,
		TLSHandshakeTimeout: settings.TlsTimeout,
		// keep the (typically single) DoH connection pooled across bursts so lookups don't
		// re-pay a TCP+TLS handshake over the tunnel
		IdleConnTimeout: 5 * time.Minute,
	}
	if settings.DnsResolverSettings != nil {
		tr.TLSClientConfig = settings.DnsResolverSettings.TlsConfig
	}
	if tr.TLSClientConfig == nil {
		tc, err := DefaultTlsConfig()
		if err != nil {
			panic(fmt.Sprintf("doh: could not build pinned TLS config: %v", err))
		}
		// per-path TLS session resumption: a separate client session cache per
		// remote/local path (upstream dohServerWarmStagger / resumption) lets the
		// h2 connection reuse tickets without cross-path linkage. capacity 16
		// matches upstream dohTlsSessionCacheCapacity.
		tc = tc.Clone()
		tc.ClientSessionCache = tls.NewLRUClientSessionCache(16)
		tr.TLSClientConfig = tc
	} else {
		// caller-supplied config (tests): still give it resumption unless it
		// already set its own cache
		if tr.TLSClientConfig.ClientSessionCache == nil {
			c := tr.TLSClientConfig.Clone()
			c.ClientSessionCache = tls.NewLRUClientSessionCache(16)
			tr.TLSClientConfig = c
		}
	}
	// most doh providers discontinued http1.1 late 2025; force h2 instead of the default
	// h1->h2 autonegotiate, since that no longer works.
	// see https://quad9.net/news/blog/doh-http-1-1-retirement/
	// ConfigureTransports (plural) returns the h2 transport so we can keep the connection
	// warm: ReadIdleTimeout sends keepalive PINGs while idle, which both holds the pooled
	// connection open across bursts and detects a dead tunnel so the next query re-dials
	// rather than stalling on a half-open connection.
	h2tr, err := http2.ConfigureTransports(tr)
	if err != nil {
		panic(err)
	}
	h2tr.ReadIdleTimeout = 30 * time.Second
	h2tr.PingTimeout = 15 * time.Second
	httpClient := &http.Client{
		Timeout:   settings.RequestTimeout,
		Transport: tr,
	}
	return httpClient
}

// sharedDohCacheMu guards access to sharedDohCacheVal, a persistent DohCache
// set by the provider at startup. When non-nil, lookupProxyTarget in net.go
// resolves proxy target hostnames through it, gaining serve-stale, server
// scoring, and lifecycle integration.
var (
	sharedDohCacheMu  sync.Mutex
	sharedDohCacheVal *DohCache
)

// SetSharedDohCache registers a persistent DohCache for proxy target
// resolution. Safe to call at startup before any proxy dials.
func SetSharedDohCache(c *DohCache) {
	sharedDohCacheMu.Lock()
	sharedDohCacheVal = c
	sharedDohCacheMu.Unlock()
}

type DohCache struct {
	httpClient      *http.Client
	localHttpClient *http.Client
	remoteResolver  *net.Resolver
	localResolver   *net.Resolver
	settings        *DohSettings
	log             Logger

	stateLock             sync.Mutex
	queryResultExpiration map[DohKey]*DohResult
	// in-flight resolutions keyed by query: concurrent identical queries (retry storms, the
	// A/AAAA split, multi-client dups) coalesce onto one resolution (single-flight). guarded
	// by stateLock.
	inflight map[DohKey]*dohFlight

	// stats ranks DoH servers by recent success so the fan-out order favors fast/recently
	// healthy servers; shared by the remote + local clients (one DohCache view).
	stats *serverStats

	// staleServeCount counts how many lookups were answered from a recently-expired
	// cache entry (RFC 8767 serve-stale) because the live resolution failed; a
	// provider can watch this to see resolver health degrading.
	staleServeCount atomic.Uint64

	// bounds concurrent resolutions so a flood of distinct names can't fan out unbounded
	resolveSem chan struct{}

	// memoryTarget is an optional global memory budget drawn per in-flight DoH
	// query (see dohQueryReserveByteCount). nil = unbounded.
	memoryTarget MemoryTarget

	// lifecycleCtx is cancelled by Close(); in-flight DoH queries are derived
	// from it so a Close aborts them (and the merged h2 connection) promptly
	// instead of leaving goroutines parked until their own timeout.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closeOnce       sync.Once
}

// dohFlight is one in-flight resolution shared by every caller waiting on the same query. the
// leader resolves, sets addrs/authoritative, then closes done to release the waiters.
type dohFlight struct {
	done          chan struct{}
	addrs         []netip.Addr
	authoritative bool
}

func dnsResolverAddrs(settings *DohSettings, remote bool, network string) []string {
	var ipv4 []string
	var ipv6 []string
	if remote {
		ipv4 = settings.DnsResolverSettings.RemoteDnsIpv4
		ipv6 = settings.DnsResolverSettings.RemoteDnsIpv6
	} else {
		ipv4 = settings.DnsResolverSettings.LocalDnsIpv4
		ipv6 = settings.DnsResolverSettings.LocalDnsIpv6
	}

	switch {
	case strings.HasSuffix(network, "6") || settings.IpVersion == 6:
		if 0 < len(ipv6) {
			return ipv6
		}
		return ipv4
	case strings.HasSuffix(network, "4") || settings.IpVersion == 4:
		if 0 < len(ipv4) {
			return ipv4
		}
		return ipv6
	default:
		addrs := append([]string{}, ipv4...)
		return append(addrs, ipv6...)
	}
}

func netIPAddr(ip net.IP) (netip.Addr, bool) {
	if ip4 := ip.To4(); ip4 != nil {
		addr, ok := netip.AddrFromSlice(ip4)
		return addr, ok
	}
	if ip16 := ip.To16(); ip16 != nil {
		addr, ok := netip.AddrFromSlice(ip16)
		return addr, ok
	}
	return netip.Addr{}, false
}

func authoritativeDnsMiss(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func NewDohCache(settings *DohSettings) *DohCache {
	remoteResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			localAddrs := dnsResolverAddrs(settings, true, network)
			if len(localAddrs) == 0 {
				return nil, fmt.Errorf("no remote DNS resolvers configured")
			}
			localAddr := localAddrs[mathrand.Intn(len(localAddrs))]
			addr = net.JoinHostPort(localAddr, port)
			return settings.DialContext(ctx, network, addr)
		},
	}

	netDialer := settings.NetDialer()
	localResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			localAddrs := dnsResolverAddrs(settings, false, network)
			if len(localAddrs) == 0 {
				return nil, fmt.Errorf("no local DNS resolvers configured")
			}
			localAddr := localAddrs[mathrand.Intn(len(localAddrs))]
			addr = net.JoinHostPort(localAddr, port)
			return netDialer.DialContext(ctx, network, addr)
		},
	}

	maxResolutions := settings.MaxConcurrentResolutions
	if maxResolutions <= 0 {
		maxResolutions = 64
	}

	stats := newServerStats()
	stats.seed(settings.ServerStatsSeed)

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	return &DohCache{
		httpClient:            httpClientWithSettings(settings),
		localHttpClient:       httpClientWithDialer(settings, settings.NetDialer().DialContext),
		remoteResolver:        remoteResolver,
		localResolver:         localResolver,
		settings:              settings,
		log:                   loggerOrDefault(settings.Log),
		queryResultExpiration: map[DohKey]*DohResult{},
		inflight:              map[DohKey]*dohFlight{},
		stats:                 stats,
		resolveSem:            make(chan struct{}, maxResolutions),
		memoryTarget:          settings.MemoryTarget,
		lifecycleCtx:          lifecycleCtx,
		lifecycleCancel:       lifecycleCancel,
	}
}

// ServerScores returns the per-server success scores driving the fan-out order,
// for the owner to persist and pass back as ServerStatsSeed on the next
// construction (the remote and local clients share one stats table, so this is
// the cache's full view).
func (self *DohCache) ServerScores() map[string]float64 {
	return self.stats.scores()
}

// Warm performs a best-effort startup probe against the configured DoH servers
// for a synthetic domain so the server scorer (P2-2) has signal before real
// traffic arrives (upstream DohPathWarm). It runs in the background and never
// blocks startup or fails it; results only influence fan-out ordering. Safe to
// call once after construction.
func (self *DohCache) Warm() {
	warmCtx, warmCancel := context.WithCancel(self.lifecycleCtx)
	go func() {
		defer warmCancel()
		timer := time.NewTimer(0)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-warmCtx.Done():
			return
		}
		// probe the top servers (already ranked by any seed) rather than every
		// configured server to keep the warm-up cheap.
		servers := remoteDohServers(self.settings, self.settings.IpVersion)
		if len(servers) == 0 {
			return
		}
		orderedUrls := urlsForServers(servers)
		if self.stats != nil {
			orderedUrls = self.stats.order(orderedUrls)
		}
		// cap the warm set
		const dohWarmMax = 4
		if len(orderedUrls) > dohWarmMax {
			orderedUrls = orderedUrls[:dohWarmMax]
		}
		serversByUrl := make(map[string]DohServer, len(servers))
		for _, s := range servers {
			serversByUrl[s.Url] = s
		}
		for _, url := range orderedUrls {
			select {
			case <-warmCtx.Done():
				return
			default:
			}
			server := serversByUrl[url]
			// the side effect of feeding stats.record() is what we want; the
			// result itself is discarded.
			_ = dohQueryWithClientResult(warmCtx, self.httpClient, self.stats,
				[]DohServer{server}, self.settings.IpVersion,
				"A", self.settings, dohWarmDomain)
		}
	}()
}

// Close shuts the cache down: it cancels any in-flight DoH queries (derived from
// lifecycleCtx) and closes the idle HTTP connections so a provider restart /
// reconfig doesn't leave goroutines parked on half-open tunnels or late h2
// connections installing after shutdown. Idempotent.
func (self *DohCache) Close() {
	self.closeOnce.Do(func() {
		self.lifecycleCancel()
		self.httpClient.CloseIdleConnections()
		self.localHttpClient.CloseIdleConnections()
	})
}

// pruneCacheLocked prunes the result cache to its configured size bound and
// drops entries that are no longer reachable: expired AND no longer
// serve-stale (beyond dohStaleServeBound). A successful resolve on any other
// key calls this for the WHOLE cache, so the condition must match the hit
// check in QueryResult: only drop entries that aren't currently serving
// traffic (or eligible to) — otherwise an unrelated successful resolve
// silently defeats serve-stale (RFC 8767) for every other key. The caller
// must hold stateLock.
func (self *DohCache) pruneCacheLocked(now time.Time, reserve int) {
	// Phase 1: delete expired + stale entries (O(n))
	for key, result := range self.queryResultExpiration {
		if !result.Valid(now, self.settings.MissExpiration) && !result.staleUsable(now) {
			delete(self.queryResultExpiration, key)
		}
	}

	maxEntries := self.settings.CacheMaxEntries
	excess := len(self.queryResultExpiration) + reserve - maxEntries
	if excess <= 0 {
		return
	}

	// Phase 2: collect remaining entries, sort by time, evict oldest.
	// O(n log n) instead of the previous O(n²) repeated full-scan loop.
	type timedEntry struct {
		key  DohKey
		time time.Time
	}
	entries := make([]timedEntry, 0, len(self.queryResultExpiration))
	for key, result := range self.queryResultExpiration {
		entries = append(entries, timedEntry{key, result.Time})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].time.Before(entries[j].time)
	})
	for i := 0; i < excess && i < len(entries); i++ {
		delete(self.queryResultExpiration, entries[i].key)
	}
}

// Query resolves a record to addresses, returning an empty slice both on an authoritative
// no-record answer and on a resolution failure. Use QueryResult to tell the two apart.
func (self *DohCache) Query(ctx context.Context, recordType string, domain string) []netip.Addr {
	addrs, _ := self.QueryResult(ctx, recordType, domain)
	return addrs
}

// QueryResult resolves a record and reports whether the answer was authoritative. authoritative
// is true when the resolver returned records or an authoritative no-record answer (NXDOMAIN /
// NODATA), and false when the resolution failed (timeout, ctx canceled, all resolvers errored)
// — a caller can map false+empty to SERVFAIL so a client retries instead of treating it as an
// authoritative "no address". Concurrent identical queries are coalesced onto one resolution
// (single-flight), and concurrent resolutions are bounded (MaxConcurrentResolutions).
func (self *DohCache) QueryResult(ctx context.Context, recordType string, domain string) ([]netip.Addr, bool) {
	q := NewDohKey(recordType, domain)
	now := time.Now()

	var fl *dohFlight
	var leader bool
	var hit bool
	var hitAddrs []netip.Addr
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if r := self.queryResultExpiration[q]; r != nil {
			if r.Valid(now, self.settings.MissExpiration) {
				hit = true
				hitAddrs = r.Addrs()
				return
			}
			// expired but still within the serve-stale window: keep it so a
			// failed live resolution can fall back to it (RFC 8767). only
			// prune it once it ages out of dohStaleServeBound.
			if !r.staleUsable(now) {
				delete(self.queryResultExpiration, q)
			}
		}
		// single-flight: lead a new resolution for this key, or join the one already running
		if existing, ok := self.inflight[q]; ok {
			fl = existing
		} else {
			fl = &dohFlight{done: make(chan struct{})}
			self.inflight[q] = fl
			leader = true
		}
	}()
	if hit {
		return hitAddrs, true
	}

	if !leader {
		select {
		case <-fl.done:
			return fl.addrs, fl.authoritative
		case <-ctx.Done():
			return nil, false
		}
	}

	// leader: resolve once, publish to any waiters, and drop the in-flight entry
	defer func() {
		self.stateLock.Lock()
		delete(self.inflight, q)
		self.stateLock.Unlock()
		close(fl.done)
	}()
	// bound concurrent resolutions; shed (empty + non-authoritative -> SERVFAIL) if a slot is
	// not free before this caller's ctx expires
	select {
	case self.resolveSem <- struct{}{}:
		defer func() { <-self.resolveSem }()
	case <-ctx.Done():
		return nil, false
	}
	fl.addrs, fl.authoritative = self.resolve(ctx, q, now)
	// serve-stale (RFC 8767): a live resolution that failed entirely (no
	// addresses and not authoritative) should still serve a recently-expired
	// cached record if we have one, so client traffic keeps flowing through a
	// resolver outage / exit failover instead of returning SERVFAIL. An
	// authoritative miss (NXDOMAIN/NODATA) is never served stale.
	if len(fl.addrs) == 0 && !fl.authoritative {
		self.stateLock.Lock()
		if r := self.queryResultExpiration[q]; r != nil && r.staleUsable(now) {
			fl.addrs = r.Addrs()
			fl.authoritative = false
			self.staleServeCount.Add(1)
		}
		self.stateLock.Unlock()
	}
	return fl.addrs, fl.authoritative
}

// resolve runs the resolver chain (remote DoH -> local DoH -> remote DNS -> local DNS) for one
// query, caches an authoritative result, and returns the addresses plus whether the answer was
// authoritative. it is not single-flighted itself; QueryResult coalesces concurrent callers.
func (self *DohCache) resolve(ctx context.Context, q DohKey, now time.Time) ([]netip.Addr, bool) {
	// derive the DoH query context from the caller ctx AND the cache lifecycle,
	// so a Close() aborts in-flight queries promptly (P3-3). context.AfterFunc
	// cancels the query when lifecycleCtx ends; the caller's ctx still bounds
	// each query normally.
	dohCtx, dohCancel := context.WithCancel(ctx)
	if self.lifecycleCtx != nil {
		context.AfterFunc(self.lifecycleCtx, dohCancel)
	}
	defer dohCancel()

	addrExpirations := map[netip.Addr]time.Time{}
	cacheMiss := false
	minCacheTtl := self.settings.MinCacheTtl

	if self.settings.DnsResolverSettings.EnableRemoteDoh {
		queryResult := dohQueryWithClientResult(dohCtx, self.httpClient, self.stats, remoteDohServers(self.settings, self.settings.IpVersion), self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableLocalDoh {
		queryResult := dohQueryWithClientResult(dohCtx, self.localHttpClient, self.stats, localDohServers(self.settings, self.settings.IpVersion), self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableRemoteDns {
		// try the remote resolver
		resolvedIps, err := self.remoteResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			found := false
			for _, ip := range resolvedIps {
				if addr, ok := netIPAddr(ip); ok {
					addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
					found = true
				}
			}
			if !found {
				cacheMiss = true
			}
		} else if authoritativeDnsMiss(err) {
			cacheMiss = true
		} else {
			dohFailureCount.Add(1)
			if ok, suppressed := shouldLogDohErr(); ok {
				if suppressed > 100 {
					self.log.Infof("🚨 [doh] %d failures in last window — last: remote (%s) err = %s\n", suppressed+1, q.Domain, err)
				} else if suppressed > 0 {
					self.log.Infof("🌐 [doh] remote (%s) err = %s (%d suppressed)\n", q.Domain, err, suppressed)
				} else {
					self.log.Infof("🌐 [doh] remote (%s) err = %s\n", q.Domain, err)
				}
			}
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableLocalDns {
		// try the local resolver
		resolvedIps, err := self.localResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			found := false
			for _, ip := range resolvedIps {
				if addr, ok := netIPAddr(ip); ok {
					addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
					found = true
				}
			}
			if !found {
				cacheMiss = true
			}
		} else if authoritativeDnsMiss(err) {
			cacheMiss = true
		} else {
			dohFailureCount.Add(1)
			if ok, suppressed := shouldLogDohErr(); ok {
				if suppressed > 0 {
					self.log.Infof("🌐 [doh] local (%s) err = %s (%d suppressed)\n", q.Domain, err, suppressed)
				} else {
					self.log.Infof("🌐 [doh] local (%s) err = %s\n", q.Domain, err)
				}
			}
		}
	}

	authoritative := 0 < len(addrExpirations) || cacheMiss
	if ctx.Err() == nil && authoritative {
		r := &DohResult{
			Time:            now,
			AddrExpirations: addrExpirations,
			Miss:            cacheMiss && len(addrExpirations) == 0,
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			self.pruneCacheLocked(now, 1)
			self.queryResultExpiration[q] = r
		}()
	}

	return (&DohResult{
		Time:            now,
		AddrExpirations: addrExpirations,
	}).Addrs(), authoritative
}

func DohQueryWithDefaults(ctx context.Context, recordType string, domains ...string) map[netip.Addr]int {
	return DohQuery(ctx, 0, recordType, DefaultDohSettings(), domains...)
}

// return ip -> ttl (seconds)
// use `ipVersion=0` to try all versions
func DohQuery(ctx context.Context, ipVersion int, recordType string, settings *DohSettings, domains ...string) map[netip.Addr]int {
	httpClient := httpClientWithSettings(settings)
	defer httpClient.CloseIdleConnections()

	return DohQueryWithClient(
		ctx,
		httpClient,
		ipVersion,
		recordType,
		settings,
		domains...,
	)
}

func DohQueryWithClient(
	ctx context.Context,
	httpClient *http.Client,
	ipVersion int,
	recordType string,
	settings *DohSettings,
	domains ...string,
) map[netip.Addr]int {
	return dohQueryWithClientResult(ctx, httpClient, nil, remoteDohServers(settings, ipVersion), ipVersion, recordType, settings, domains...).AddrTtls
}

func dohServersFor(ipv4 []DohServer, ipv6 []DohServer, ipVersion int) []DohServer {
	switch ipVersion {
	case 4:
		return ipv4
	case 6:
		return ipv6
	default:
		servers := append([]DohServer{}, ipv4...)
		return append(servers, ipv6...)
	}
}

// legacyDohServers adapts the format-less *DohUrls []string fields as json servers.
func legacyDohServers(urls []string) []DohServer {
	servers := make([]DohServer, len(urls))
	for i, u := range urls {
		servers[i] = DohServer{Url: u, Format: DohFormatJson}
	}
	return servers
}

// remoteDohServers/localDohServers return the tagged servers for the ip version, with the legacy
// *DohUrls fields appended as json servers.
func remoteDohServers(settings *DohSettings, ipVersion int) []DohServer {
	rs := settings.DnsResolverSettings
	servers := append([]DohServer{}, dohServersFor(rs.RemoteDohServersIpv4, rs.RemoteDohServersIpv6, ipVersion)...)
	return append(servers, dohServersFor(legacyDohServers(rs.RemoteDohUrlsIpv4), legacyDohServers(rs.RemoteDohUrlsIpv6), ipVersion)...)
}

func localDohServers(settings *DohSettings, ipVersion int) []DohServer {
	rs := settings.DnsResolverSettings
	servers := append([]DohServer{}, dohServersFor(rs.LocalDohServersIpv4, rs.LocalDohServersIpv6, ipVersion)...)
	return append(servers, dohServersFor(legacyDohServers(rs.LocalDohUrlsIpv4), legacyDohServers(rs.LocalDohUrlsIpv6), ipVersion)...)
}

// serverStats tracks each DoH server's recent success rate over trailing time
// windows so the fan-out order favors servers that have resolved most recently
// and most often. A dead server sinks to the exploration floor; a recovered one
// climbs back as live successes accrue. P2-2 (ported from upstream).
type serverStats struct {
	lock  sync.Mutex
	byUrl map[string]*serverStat
}

func newServerStats() *serverStats {
	return &serverStats{byUrl: map[string]*serverStat{}}
}

// dohServerWindows are the trailing spans over which each server's successful
// resolutions are counted (parallel index into serverStat.windows).
var dohServerWindows = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

// dohServerWeightFloor keeps every server a small chance of being tried first
// (exploration), so a server that recovers can climb back even after a streak
// of failures.
const dohServerWeightFloor = 0.05

// dohSeedMaxScore clamps a persisted per-server score on seed: high enough to
// make the last session's fastest server the clear first pick, low enough that
// a few live successes on another server can overturn a stale ordering.
const dohSeedMaxScore = 8.0

// dohStaleServeBound is how long after a record's TTL a previously-resolved
// address may still be served when a live resolution fails (RFC 8767
// serve-stale). This keeps client traffic flowing through a tunnel during a
// resolver outage / exit failover instead of hard-failing every lookup. Only
// address records are served stale — an authoritative miss (NXDOMAIN/NODATA)
// is never served stale, because serving a stale "this name exists" would be
// wrong once the name is actually withdrawn.
const dohStaleServeBound = 5 * time.Minute

const dohWarmDomain = "example.com"

// dohQueryReserveByteCount is the memory reserved per in-flight DoH query so the
// resolver's memory use scales with concurrency instead of spiking unbounded
// during a retry storm. Mirrors upstream; released when the query returns.
const dohQueryReserveByteCount = 16 * 1024

// MemoryTarget is an optional global memory budget the DoH cache draws from so
// resolution memory stays bounded under load. Acquire blocks until the bytes are
// available (or ctx ends); Release returns them. A nil *DohCache.memoryTarget is
// a no-op (unbounded). The provider supplies a real implementation (e.g. a
// fraction of container/cgroup memory) if it wants a hard cap.
type MemoryTarget interface {
	Acquire(ctx context.Context, bytes int64) bool
	Release(bytes int64)
}

// ByteBudget is a simple channel-backed MemoryTarget: it admits acquisitions
// while at least `capacity` bytes are unconsumed, blocking (via ctx) once full.
// Acquire returns false if ctx ends before bytes free up, so a caller can shed
// instead of deadlocking under memory pressure.
type ByteBudget struct {
	used atomic.Int64
	cap  int64
	wait chan struct{}
}

func NewByteBudget(capacity int64) *ByteBudget {
	return &ByteBudget{cap: capacity, wait: make(chan struct{}, 1)}
}

func (self *ByteBudget) Acquire(ctx context.Context, bytes int64) bool {
	if bytes > self.cap {
		return false
	}
	for {
		cur := self.used.Load()
		if cur+bytes <= self.cap {
			if self.used.CompareAndSwap(cur, cur+bytes) {
				return true
			}
			continue
		}
		// full: wait for a release or ctx end
		select {
		case <-ctx.Done():
			return false
		case <-self.wait:
		}
	}
}

func (self *ByteBudget) Release(bytes int64) {
	self.used.Add(-bytes)
	// nudge one waiter (if any); non-blocking
	select {
	case self.wait <- struct{}{}:
	default:
	}
}

// serverStat holds one tokenBucket per dohServerWindows span (parallel index),
// counting the server's successful resolutions.
type serverStat struct {
	windows []tokenBucket
}

// tokenBucket is a sliding-window-counter approximation over a fixed span:
// current counts events in the current interval; previous the interval before
// it. The trailing-window estimate prorates previous by how much of it still
// falls within the last span.
type tokenBucket struct {
	epoch    int64
	current  float64
	previous float64
}

// roll advances the bucket to the interval containing now: a single-interval
// step shifts current->previous; a longer gap clears both (events fell out).
func (self *tokenBucket) roll(span time.Duration, now time.Time) {
	epoch := now.UnixNano() / int64(span)
	switch {
	case epoch == self.epoch:
	case epoch == self.epoch+1:
		self.previous = self.current
		self.current = 0
	default:
		self.previous = 0
		self.current = 0
	}
	self.epoch = epoch
}

func (self *tokenBucket) add(span time.Duration, now time.Time, n float64) {
	self.roll(span, now)
	self.current += n
}

// estimate returns the prorated event count over the trailing span ending at now.
func (self *tokenBucket) estimate(span time.Duration, now time.Time) float64 {
	self.roll(span, now)
	elapsed := now.UnixNano() - self.epoch*int64(span)
	frac := float64(int64(span)-elapsed) / float64(span)
	return self.current + self.previous*frac
}

// record credits a server with a successful resolution (ok == it returned
// records or an authoritative no-record answer); failures earn nothing and
// simply let the buckets decay.
func (self *serverStats) record(url string, ok bool) {
	self.recordAt(url, ok, time.Now())
}

func (self *serverStats) recordAt(url string, ok bool, now time.Time) {
	if self == nil || !ok {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()

	st := self.byUrl[url]
	if st == nil {
		st = &serverStat{windows: make([]tokenBucket, len(dohServerWindows))}
		self.byUrl[url] = st
	}
	for k, span := range dohServerWindows {
		st.windows[k].add(span, now, 1)
	}
}

// seed pre-loads each server's windows with a persisted score (clamped to
// dohSeedMaxScore), spread evenly across the windows so the summed score
// matches and decays on the normal trailing-window schedule. Used at
// construction to carry the fan-out ordering across a restart.
func (self *serverStats) seed(scores map[string]float64) {
	if self == nil || len(scores) == 0 {
		return
	}
	now := time.Now()
	self.lock.Lock()
	defer self.lock.Unlock()
	for url, score := range scores {
		if score <= 0 {
			continue
		}
		score = min(score, dohSeedMaxScore)
		st := self.byUrl[url]
		if st == nil {
			st = &serverStat{windows: make([]tokenBucket, len(dohServerWindows))}
			self.byUrl[url] = st
		}
		for k, span := range dohServerWindows {
			st.windows[k].add(span, now, score/float64(len(dohServerWindows)))
		}
	}
}

// scores returns each known server's current summed trailing-window success
// estimate (the fan-out order weights), for the owner to persist and pass back
// as ServerStatsSeed on the next construction. Zero-score servers are omitted.
func (self *serverStats) scores() map[string]float64 {
	if self == nil {
		return nil
	}
	now := time.Now()
	self.lock.Lock()
	defer self.lock.Unlock()
	scores := map[string]float64{}
	for url := range self.byUrl {
		if score := self.scoreLocked(url, now); 0 < score {
			scores[url] = score
		}
	}
	return scores
}

// scoreLocked sums a server's trailing-window success estimates; untried = 0.
func (self *serverStats) scoreLocked(url string, now time.Time) float64 {
	st := self.byUrl[url]
	if st == nil {
		return 0
	}
	var score float64
	for k, span := range dohServerWindows {
		score += st.windows[k].estimate(span, now)
	}
	return score
}

// order returns urls in a weighted-random permutation: a server's weight is its
// summed recent success score plus an exploration floor. Uses the
// Efraimidis–Spirakis weighted-permutation method (key = u^(1/w); higher weight
// -> earlier). A nil *serverStats yields a uniform-random shuffle.
func (self *serverStats) order(urls []string) []string {
	return self.orderAt(urls, time.Now())
}

func (self *serverStats) orderAt(urls []string, now time.Time) []string {
	ordered := append([]string{}, urls...)
	if len(ordered) <= 1 {
		return ordered
	}
	if self == nil {
		mathrand.Shuffle(len(ordered), func(i, j int) {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		})
		return ordered
	}

	type weighted struct {
		url string
		key float64
	}
	ws := make([]weighted, len(ordered))
	self.lock.Lock()
	for i, url := range ordered {
		weight := dohServerWeightFloor + self.scoreLocked(url, now)
		u := mathrand.Float64()
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		ws[i] = weighted{url: url, key: math.Pow(u, 1/weight)}
	}
	self.lock.Unlock()

	sort.Slice(ws, func(i, j int) bool {
		return ws[i].key > ws[j].key
	})
	for i := range ws {
		ordered[i] = ws[i].url
	}
	return ordered
}

// urlsForServers returns the server URLs in order, for the weighted-permutation
// ordering step.
func urlsForServers(servers []DohServer) []string {
	urls := make([]string, len(servers))
	for i, s := range servers {
		urls[i] = s.Url
	}
	return urls
}

// serversByUrl reorders servers to match the given url order (the output of
// serverStats.order), preserving each server's Format tag.
func serversByUrl(servers []DohServer, orderedUrls []string) []DohServer {
	byUrl := make(map[string]DohServer, len(servers))
	for _, s := range servers {
		byUrl[s.Url] = s
	}
	ordered := make([]DohServer, 0, len(orderedUrls))
	for _, u := range orderedUrls {
		if s, ok := byUrl[u]; ok {
			ordered = append(ordered, s)
		}
	}
	for _, s := range servers {
		if _, ok := byUrl[s.Url]; !ok {
			ordered = append(ordered, s)
		}
	}
	return ordered
}

type dohQueryResult struct {
	AddrTtls map[netip.Addr]int
	Miss     bool
}

func newDohQueryResult() *dohQueryResult {
	return &dohQueryResult{
		AddrTtls: map[netip.Addr]int{},
	}
}

func dohQueryWithClientResult(
	ctx context.Context,
	httpClient *http.Client,
	stats *serverStats,
	dohServers []DohServer,
	ipVersion int,
	recordType string,
	settings *DohSettings,
	domains ...string,
) *dohQueryResult {
	// run all the queries in parallel to all servers

	queryCtx, queryCancel := context.WithCancel(ctx)
	defer queryCancel()

	switch recordType {
	case "A", "AAAA":
	default:
		return newDohQueryResult()
	}

	// reserve memory from the optional budget; blocks until available (or ctx
	// ends). released on return. no-op when MemoryTarget is nil.
	if settings.MemoryTarget != nil {
		if !settings.MemoryTarget.Acquire(ctx, dohQueryReserveByteCount) {
			return newDohQueryResult()
		}
		defer settings.MemoryTarget.Release(dohQueryReserveByteCount)
	}

	query := func(server DohServer, domain string) *dohQueryResult {
		name, err := Punycode(domain)
		if err != nil {
			return newDohQueryResult()
		}
		if server.Format == DohFormatWire {
			return dohQueryWire(queryCtx, httpClient, server.Url, recordType, name)
		}
		return dohQueryJson(queryCtx, httpClient, server.Url, recordType, name)
	}

	queryCount := len(dohServers) * len(domains)
	if queryCount == 0 || settings.RequestTimeout <= 0 {
		return newDohQueryResult()
	}
	receiveResults := make(chan *dohQueryResult, queryCount)
	// stop launches the moment a winning answer arrives (fastest-record-wins):
	// pending goroutines finish naturally, their results are discarded via select.
	stop := make(chan struct{})
	var stopOnce sync.Once
	// stopLaunching halts further launches AND cancels in-flight losers the
	// moment a winning answer arrives (fastest-record-wins): a slow or dead
	// server must not burn its full RequestTimeout after a winner is known.
	// Cancelling queryCtx does not corrupt scoring: record() only credits
	// successes (ok==true), and a cancelled loser returns an empty result
	// (Miss=false) so it is simply not scored, keeping its prior score.
	stopLaunching := func() { stopOnce.Do(func() { close(stop); queryCancel() }) }
	defer stopLaunching()

	// weighted-random fan-out order: fast/recently-successful servers tend to
	// fire first; a dead server sinks to the exploration floor. stats may be nil
	// (one-shot queries) -> uniform-random order via order().
	orderedServers := dohServers
	if len(dohServers) > 1 && stats != nil {
		orderedServers = serversByUrl(dohServers, stats.order(urlsForServers(dohServers)))
	}

	stagger := settings.DohServerStagger
	// launch servers in weighted order, one per stagger wave; a prior wave's
	// answer (or any winning record) closes stop so later servers never fire.
	launchCtx, launchCancel := context.WithCancel(queryCtx)
	defer launchCancel()
	go HandleError(func() {
		for i := range orderedServers {
			// stagger gates the delay BEFORE launching server i; a winner
			// (stop) or cancellation aborts further waves. once we're past
			// the delay, server i is committed to launch — we must not skip
			// it just because a winner arrived, or a stagger=0 (all-at-once)
			// launch would drop peers that hadn't fired yet.
			if 0 < i && 0 < stagger {
				select {
				case <-time.After(stagger):
				case <-stop:
					return
				case <-launchCtx.Done():
					return
				}
			}
			for _, domain := range domains {
				// capture the exact server for this goroutine: the loop variable is
				// reused across iterations, so we must not close over it by reference
				srv := orderedServers[i]
				go HandleError(func() {
					result := query(srv, domain)
					// record server success for future fan-out ordering
					if stats != nil {
						stats.record(srv.Url, 0 < len(result.AddrTtls) || result.Miss)
					}
					select {
					case receiveResults <- result:
					case <-stop:
					case <-launchCtx.Done():
					}
				})
			}
		}
	})

	endTime := time.Now().Add(settings.RequestTimeout)
	mergedResult := newDohQueryResult()
	for range queryCount {
		timeout := endTime.Sub(time.Now())
		if timeout <= 0 {
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
				Miss:     mergedResult.Miss,
			}
		}
		select {
		case <-queryCtx.Done():
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
				Miss:     mergedResult.Miss,
			}
		case result := <-receiveResults:
			maps.Copy(mergedResult.AddrTtls, result.AddrTtls)
			if result.Miss {
				mergedResult.Miss = true
			}
			if 0 < len(mergedResult.AddrTtls) {
				stopLaunching()
				return &dohQueryResult{
					AddrTtls: mergedResult.AddrTtls,
				}
			}
		case <-time.After(timeout):
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
				Miss:     mergedResult.Miss,
			}
		}
	}
	mergedResult.Miss = len(mergedResult.AddrTtls) == 0 && mergedResult.Miss
	return mergedResult
}

// dohQueryJson runs a Google-style JSON DoH query (Accept application/dns-json, GET ?name=&type=).
// name must already be punycoded ascii.
func dohQueryJson(ctx context.Context, httpClient *http.Client, dohUrl string, recordType string, name string) *dohQueryResult {
	result := newDohQueryResult()

	params := url.Values{}
	params.Add("name", name)
	params.Add("type", recordType)
	requestUrl := fmt.Sprintf("%s?%s", dohUrl, params.Encode())

	request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
	if err != nil {
		return result
	}
	request.Header.Set("Accept", "application/dns-json")
	// note, we do not set the User-Agent for DoH requests
	// see https://bugzilla.mozilla.org/show_bug.cgi?id=1543201#c4

	response, err := httpClient.Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return result
	}

	dohResponse := &DohResponse{}
	if err := json.Unmarshal(data, dohResponse); err != nil {
		return result
	}
	switch dohResponse.Status {
	case 0:
	case 3:
		result.Miss = true
		return result
	default:
		return result
	}
	for _, answer := range dohResponse.Answer {
		if ip, err := netip.ParseAddr(answer.Data); err == nil {
			result.AddrTtls[ip] = max(result.AddrTtls[ip], answer.TTL)
		}
	}
	if len(result.AddrTtls) == 0 {
		result.Miss = true
	}
	return result
}

// dohQueryWire runs an RFC 8484 wire-format DoH query (Accept application/dns-message,
// GET ?dns=<base64url DNS message>). name must already be punycoded ascii.
func dohQueryWire(ctx context.Context, httpClient *http.Client, dohUrl string, recordType string, name string) *dohQueryResult {
	result := newDohQueryResult()

	dnsName, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return result
	}
	var qType dnsmessage.Type
	switch recordType {
	case "A":
		qType = dnsmessage.TypeA
	case "AAAA":
		qType = dnsmessage.TypeAAAA
	default:
		return result
	}
	// id 0 is recommended for DoH (RFC 8484 §4.1); recursion desired
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsName, Type: qType, Class: dnsmessage.ClassINET}},
	}
	wire, err := msg.Pack()
	if err != nil {
		return result
	}
	requestUrl := fmt.Sprintf("%s?dns=%s", dohUrl, base64.RawURLEncoding.EncodeToString(wire))

	request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
	if err != nil {
		return result
	}
	request.Header.Set("Accept", "application/dns-message")

	response, err := httpClient.Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return result
	}
	return parseDohWire(data, qType)
}

// parseDohWire parses an RFC 8484 wire-format DNS response, extracting the A or AAAA answers
// matching qType. NXDOMAIN -> Miss; other non-success RCODEs -> failure (empty, not Miss).
func parseDohWire(data []byte, qType dnsmessage.Type) *dohQueryResult {
	result := newDohQueryResult()

	var p dnsmessage.Parser
	header, err := p.Start(data)
	if err != nil {
		return result
	}
	switch header.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError: // NXDOMAIN
		result.Miss = true
		return result
	default:
		return result
	}
	if err := p.SkipAllQuestions(); err != nil {
		return result
	}
	for {
		ah, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return result
		}
		switch {
		case ah.Type == dnsmessage.TypeA && qType == dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return result
			}
			ip := netip.AddrFrom4(r.A)
			result.AddrTtls[ip] = max(result.AddrTtls[ip], int(ah.TTL))
		case ah.Type == dnsmessage.TypeAAAA && qType == dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return result
			}
			ip := netip.AddrFrom16(r.AAAA)
			result.AddrTtls[ip] = max(result.AddrTtls[ip], int(ah.TTL))
		default:
			if err := p.SkipAnswer(); err != nil {
				return result
			}
		}
	}
	if len(result.AddrTtls) == 0 {
		result.Miss = true
	}
	return result
}

type DohKey struct {
	RecordType string
	Domain     string
}

func NewDohKey(recordType string, domain string) DohKey {
	return DohKey{
		RecordType: strings.ToUpper(recordType),
		Domain:     strings.ToLower(domain),
	}
}

type DohResult struct {
	Time            time.Time
	AddrExpirations map[netip.Addr]time.Time
	Miss            bool
}

// Valid reports whether the result can still be served at now: a result
// holding addresses is served while any address is not yet expired; a result
// holding none is served only for an authoritative miss (Miss) recorded
// within missExpiration. The exclusivity of the miss bound matters: it is
// what makes a cached NXDOMAIN/NODATA answer expire instead of persisting
// forever.
func (self *DohResult) Valid(now time.Time, missExpiration time.Duration) bool {
	if len(self.AddrExpirations) == 0 {
		return self.Miss && !self.Time.Add(missExpiration).Before(now)
	}
	for _, expireTime := range self.AddrExpirations {
		if expireTime.Before(now) {
			return false
		}
	}
	return true
}

// staleUsable reports whether this (now-expired) result may still be served as
// a stale answer: it must hold addresses (never a miss) and the records must
// have expired within dohStaleServeBound. Used by serve-stale (RFC 8767) when a
// live resolution fails, so a recently-cached address keeps working through a
// resolver outage instead of returning SERVFAIL.
func (self *DohResult) staleUsable(now time.Time) bool {
	if len(self.AddrExpirations) == 0 {
		return false
	}
	return self.Time.Add(dohStaleServeBound).After(now)
}

// Addrs returns the result's addresses as a slice; the order is unspecified
// because the addresses are stored in a map. The slice is empty for a miss or
// an address-less result.
func (self *DohResult) Addrs() []netip.Addr {
	ips := []netip.Addr{}
	for ip := range self.AddrExpirations {
		ips = append(ips, ip)
	}
	return ips
}

type DohQuestion struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

type DohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

type DohResponse struct {
	Status   int           `json:"Status"`
	TC       bool          `json:"TC"`
	RD       bool          `json:"RD"`
	RA       bool          `json:"RA"`
	AD       bool          `json:"AD"`
	CD       bool          `json:"CD"`
	Question []DohQuestion `json:"Question"`
	Answer   []DohAnswer   `json:"Answer"`
}

func Punycode(domain string) (string, error) {
	name := strings.TrimSpace(domain)

	return idna.New(
		idna.MapForLookup(),
		idna.Transitional(true),
		idna.StrictDomainName(false),
	).ToASCII(name)
}
