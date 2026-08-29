package connect

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

type DialContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)
type DialTlsContextFunction = func(ctx context.Context, network string, addr string) (net.Conn, error)

// dnsCacheEntry holds a resolved IP plus its expiry time.
type dnsCacheEntry struct {
	ip     string
	expiry time.Time
}

// dnsCache caches hostname-to-IPv4 lookups for proxy SOCKS5 CONNECT
// targets, so the system resolver isn't hit on every single dial through
// every proxy. TTL is 60 seconds — long enough to absorb burst dials from
// 2000+ concurrent warmup goroutines, short enough to pick up DNS changes.
var dnsCache struct {
	mu sync.Mutex
	m  map[string]dnsCacheEntry
}

const dnsCacheTTL = 60 * time.Second

// lookupProxyTarget resolves a proxy target hostname to an IPv4 address.
// When a shared DohCache has been registered (via SetSharedDohCache), DNS
// resolution goes through the cache rather than net.DefaultResolver — gaining
// serve-stale (RFC 8767) during resolver outages, server-scoring-based fan-out
// ordering, and lifecycle integration.
func lookupProxyTarget(host string) (string, bool) {
	// Try the shared DohCache first (if one is registered).
	sharedDohCacheMu.Lock()
	c := sharedDohCacheVal
	sharedDohCacheMu.Unlock()
	if c != nil {
		addrs := c.Query(context.Background(), "A", host)
		if len(addrs) > 0 {
			return addrs[0].String(), true
		}
		// Cache returned empty — fall through to the local resolver + stale
		// cache path below.
	}

	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	if dnsCache.m == nil {
		dnsCache.m = make(map[string]dnsCacheEntry)
	}
	e, ok := dnsCache.m[host]
	if ok && time.Now().Before(e.expiry) {
		return e.ip, true
	}
	ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip4", host)
	if err != nil || len(ips) == 0 {
		if ok {
			// Stale entry is better than nothing — return it if the
			// lookup failed, so a transient resolver blip doesn't
			// cause every proxy dial to fall back to the hostname.
			return e.ip, true
		}
		return "", false
	}
	ip := ips[0].String()
	dnsCache.m[host] = dnsCacheEntry{ip: ip, expiry: time.Now().Add(dnsCacheTTL)}
	return ip, true
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
		TlsConfig: tlsConfig,
	}
}

type ConnectSettings struct {
	// Log, when set, is used by the connect functions. nil resolves to
	// DefaultLogger().
	Log Logger

	RequestTimeout   time.Duration
	ConnectTimeout   time.Duration
	TlsTimeout       time.Duration
	HandshakeTimeout time.Duration
	IdleConnTimeout  time.Duration
	KeepAliveTimeout time.Duration
	KeepAliveConfig  net.KeepAliveConfig

	TlsConfig *tls.Config

	ProxySettings *ProxySettings
	Resolver      *net.Resolver

	DialContextSettings *DialContextSettings

	DisableIpv4 bool
	DisableIpv6 bool
}

type DialContextSettings struct {
	DialContext DialContextFunction
}

// DialContext dials network/addr, applying the family policy from
// DisableIpv4/DisableIpv6: explicitly requested families that are disabled
// (tcp4/tcp6/udp4/udp6) are rejected, while generic "tcp"/"udp" are remapped
// to the enabled family. The dial goes through DialContextSettings.DialContext
// when set, otherwise through ProxySettings.NewDialContext when a proxy is
// configured, otherwise through a plain NetDialer. Dial results are logged
// at V(2).
func (self *ConnectSettings) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
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
		if err == nil {
			log.Infof("[net]dial %s %s success\n", network, addr)
		} else {
			log.Infof("[net]dial %s %s err=%s\n", network, addr, err)
		}
	}
	return conn, err
}

// NetDialer builds a fresh net.Dialer from the settings' connect timeout,
// keep-alive configuration, and resolver; each call returns an independent
// dialer, so per-dial state never shares between calls.
func (self *ConnectSettings) NetDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:         self.ConnectTimeout,
		KeepAlive:       self.KeepAliveTimeout,
		KeepAliveConfig: self.KeepAliveConfig,
		Resolver:        self.Resolver,
	}
}

type ProxySettings struct {
	Index   int
	Network string
	Address string
	Auth    *proxy.Auth
}

// proxySOCKS5DialTimeout is the maximum time a single SOCKS5 dial (TCP
// connect + greeting + CONNECT) is allowed to take through a proxy. Paths
// that already carry a context deadline (e.g. serialEval with its 15s
// RequestTimeout) are unaffected — this only applies when the caller's
// context has no deadline, which happens during the startup warmup burst
// where 2000+ goroutines compete for admission and can pile up
// indefinitely.
const proxySOCKS5DialTimeout = 30 * time.Second

// NewDialContext returns a DialContextFunction that dials through this
// SOCKS5 proxy using forward as the transport. Non-IP hosts are resolved
// through the package-level dnsCache before the proxy CONNECT; when the
// caller's context carries no deadline the proxy dial is bounded by
// proxySOCKS5DialTimeout; successful connections are wrapped in a trackedConn
// that counts bytes against this proxy's bandwidth index.
func (self *ProxySettings) NewDialContext(ctx context.Context, forward proxy.Dialer) DialContextFunction {
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err == nil {
			if ip := net.ParseIP(host); ip == nil {
				if resolved, ok := lookupProxyTarget(host); ok {
					addr = net.JoinHostPort(resolved, port)
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
			return nil, err
		}

		dialCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			dialCtx, cancel = context.WithTimeout(ctx, proxySOCKS5DialTimeout)
			defer cancel()
		}

		var conn net.Conn
		if v, ok := proxyDialer.(proxy.ContextDialer); ok {
			conn, err = v.DialContext(dialCtx, network, addr)
		} else {
			conn, err = proxyDialer.Dial(network, addr)
		}
		if err != nil {
			return nil, err
		}

		bw := RegisterProxyBandwidth(self.Index)
		if bw != nil {
			tc := &trackedConn{Conn: conn, bw: bw}
			return tc, nil
		}

		return conn, nil
	}
}

type trackedConn struct {
	net.Conn
	bw   *ProxyBandwidth
	once sync.Once
}

// Read reads from the underlying connection and, when bw is set, credits the
// byte count to the proxy's TotalRx.
func (self *trackedConn) Read(b []byte) (n int, err error) {
	n, err = self.Conn.Read(b)
	if n > 0 && self.bw != nil {
		self.bw.TotalRx.Add(uint64(n))
	}
	return
}

// Write writes to the underlying connection and, when bw is set, credits the
// byte count to the proxy's TotalTx.
func (self *trackedConn) Write(b []byte) (n int, err error) {
	n, err = self.Conn.Write(b)
	if n > 0 && self.bw != nil {
		self.bw.TotalTx.Add(uint64(n))
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
