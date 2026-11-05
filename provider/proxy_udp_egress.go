package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Client udp egress for a proxied identity.
//
// The local user nat dials each client udp flow through the identity's
// connect settings. For a proxied identity those carry ProxySettings, and the
// engine's proxy dialer is golang.org/x/net/proxy's SOCKS5 client, which only
// implements CONNECT: every udp dial failed with "network not implemented"
// before the proxy was contacted, so a proxied identity dropped all tunneled
// udp (dns, stun, quic, games) while tcp worked.
//
// installProxiedUdpEgress gives the nat's udp settings a dialer that carries a
// flow through the proxy's udp relay instead (socks5_udp.go): one udp
// association per flow, returned as a net.Conn bound to the flow's destination.
// Tcp is not touched; the nat's tcp settings are a separate copy and keep the
// CONNECT path.
//
// Rules:
//   - the relay is gated on the per-proxy udp check (proxy_udp_check.go), the
//     same answer the h3 factory uses. ok and unknown are allowed (the relay is
//     the proxy either way, and unknown is checked again sooner); no-relay and
//     exit-mismatch are refused at once. The check runs detached from the
//     flow, so its result is remembered even when the flow that started it
//     gives up waiting
//   - a refused or failed flow is an error, never a socket on the host. A host
//     socket would put the identity's udp at the box's own address instead of
//     the proxy's exit. A dial error makes the nat answer the client with an
//     unreachable, so apps move on quickly
//   - ipv6 destinations are refused: the check only proves the relay over
//     ipv4, and most relays have no ipv6 exit, so a v6 datagram would vanish
//     instead of failing fast
//   - the association lives exactly as long as the flow: closing the conn
//     closes the relay socket and the association's control connection. Each
//     flow holds a tcp connection on the proxy, so proxied identities close
//     idle udp flows sooner than the nat default (proxiedUdpFlowIdleTimeout)
//   - bytes are counted by the nat the same way as a direct socket (trackDirect
//     wraps the returned conn with the identity's bandwidth counters)
//
// The returned conn is opaque (not a syscall.Conn). Two consequences, both
// intended: egress ttl realism counts these flows as socket-unavailable (a ttl
// on the hop to the proxy would mean nothing), and the nat's shared socket
// lifecycle cannot register them. That lifecycle is off for every identity
// today; enabling it for proxied identities would dial each flow twice.

const (
	// how long a flow waits for the identity's udp check before it is dialed
	// as unknown. The check is shared and runs at most once per identity, but a
	// host with many identities queues behind a bounded number of check slots
	proxiedUdpCheckWait = 20 * time.Second
	// the connect and handshake bound of one association, as the tcp path
	// bounds its proxy dials. The flow context itself has no deadline
	proxiedUdpRelayDialTimeout = 15 * time.Second
	// read and idle timeout of a proxied identity's udp flows. The nat ends a
	// flow when either direction is silent this long (read timeout for
	// replies, idle timeout for sends), so this is long enough for one-way
	// keepalives and still frees one-shot flows like dns well before the
	// 300s default
	proxiedUdpFlowIdleTimeout = 120 * time.Second
)

var (
	errProxiedUdpNoRelay       = errors.New("proxy udp check: no udp relay")
	errProxiedUdpExitMismatch  = errors.New("proxy udp check: udp exits from another address than tcp")
	errProxiedUdpBadNetwork    = errors.New("proxy udp: unsupported network")
	errProxiedUdpBadAddress    = errors.New("proxy udp: destination is not an ip address")
	errProxiedUdpIpv6Refused   = errors.New("proxy udp: ipv6 destinations are not relayed")
	errProxiedUdpFamilyRefused = errors.New("proxy udp: destination family does not match the requested network")
)

// applyIdentityNatConnectSettings gives an identity's local user nat the
// identity's connect settings, so client tcp and udp egress the way the
// identity does. For a proxied identity tcp keeps the proxy's CONNECT path and
// udp goes through the proxy's udp relay.
func applyIdentityNatConnectSettings(localUserNatSettings *connect.LocalUserNatSettings, connectSettings connect.ConnectSettings, proxySettings *connect.ProxySettings, checkStore *proxyUDPCheckStore) {
	localUserNatSettings.TcpBufferSettings.ConnectSettings = connectSettings
	localUserNatSettings.UdpBufferSettings.ConnectSettings = connectSettings
	if proxySettings == nil {
		return
	}
	installProxiedUdpEgress(&localUserNatSettings.UdpBufferSettings.ConnectSettings, proxySettings, checkStore)
	udpBufferSettings := localUserNatSettings.UdpBufferSettings
	udpBufferSettings.ReadTimeout = min(udpBufferSettings.ReadTimeout, proxiedUdpFlowIdleTimeout)
	udpBufferSettings.IdleTimeout = min(udpBufferSettings.IdleTimeout, proxiedUdpFlowIdleTimeout)
}

// installProxiedUdpEgress points the udp dials of connectSettings at the udp
// relay of proxySettings, gated by checkStore. Non-udp dials keep the dialer
// connectSettings had. A nil proxySettings (the direct identity) leaves the
// settings untouched.
func installProxiedUdpEgress(connectSettings *connect.ConnectSettings, proxySettings *connect.ProxySettings, checkStore *proxyUDPCheckStore) {
	if proxySettings == nil {
		return
	}
	// the dialer a non-udp dial would have used, resolved now so the closure
	// does not read the settings it is installed on
	var otherDialContext connect.DialContextFunction
	if previous := connectSettings.DialContextSettings; previous != nil && previous.DialContext != nil {
		otherDialContext = previous.DialContext
	} else {
		base := *connectSettings
		base.DialContextSettings = nil
		otherDialContext = func(ctx context.Context, network string, addr string) (net.Conn, error) {
			if base.ProxySettings == nil {
				return base.NetDialer().DialContext(ctx, network, addr)
			}
			return base.ProxySettings.NewDialContext(ctx, base.NetDialer())(ctx, network, addr)
		}
	}
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			switch network {
			case "udp", "udp4", "udp6":
				return dialProxiedUdp(ctx, proxySettings, checkStore, network, addr)
			default:
				return otherDialContext(ctx, network, addr)
			}
		},
	}
}

// dialProxiedUdp opens one udp association on the proxy for one flow to addr.
func dialProxiedUdp(ctx context.Context, proxySettings *connect.ProxySettings, checkStore *proxyUDPCheckStore, network string, addr string) (net.Conn, error) {
	destination, err := proxiedUdpDestination(network, addr)
	if err != nil {
		if errors.Is(err, errProxiedUdpIpv6Refused) {
			proxiedUdpRefusals.note(proxySettings, proxiedUdpRefusalIpv6)
		}
		return nil, err
	}

	// the check runs detached so a result is remembered for the identity even
	// when this flow stops waiting
	checkRun := checkStore.checkDetached(proxySettings, time.Now())
	var result proxyUDPResult
	select {
	case <-checkRun.done:
		result = checkRun.result
	case <-time.After(proxiedUdpCheckWait):
		result = proxyUDPResult{Status: proxyUDPUnknown}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch result.Status {
	case proxyUDPNoRelay:
		proxiedUdpRefusals.note(proxySettings, proxiedUdpRefusalNoRelay)
		return nil, errProxiedUdpNoRelay
	case proxyUDPExitMismatch:
		proxiedUdpRefusals.note(proxySettings, proxiedUdpRefusalExitMismatch)
		return nil, errProxiedUdpExitMismatch
	}

	relayed, err := func() (net.PacketConn, error) {
		relayCtx, cancel := context.WithTimeout(ctx, proxiedUdpRelayDialTimeout)
		defer cancel()
		return dialSocks5UDP(relayCtx, proxySettings.Network, proxySettings.Address, proxySettings.Auth)
	}()
	if err != nil {
		// as in the h3 factory: the dial error embeds the address the dialer
		// was given, so it is classified, not wrapped
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		proxiedUdpRefusals.note(proxySettings, proxiedUdpRefusalAssociation)
		return nil, fmt.Errorf("proxy udp through %s: relay dial failed: %s", redactedProxyAddress(proxySettings.Address), dialFailureClass(err))
	}
	return &proxiedUdpConn{
		relayed:     relayed,
		destination: destination,
	}, nil
}

// proxiedUdpDestination parses a flow's destination. The nat always dials an
// ip literal; a name would need a resolution that does not go through the
// proxy, so it is refused, and so is an ipv6 destination (see the file header).
func proxiedUdpDestination(network string, addr string) (*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errProxiedUdpBadAddress
	}
	port, err := net.LookupPort("udp", portText)
	if err != nil {
		return nil, err
	}
	ip4 := ip.To4()
	switch network {
	case "udp", "udp4":
	case "udp6":
		if ip4 != nil {
			return nil, errProxiedUdpFamilyRefused
		}
	default:
		return nil, errProxiedUdpBadNetwork
	}
	if ip4 == nil {
		return nil, errProxiedUdpIpv6Refused
	}
	return &net.UDPAddr{IP: ip4, Port: port}, nil
}

// proxiedUdpConn is a socks5 udp association bound to one destination. Writes
// become relay datagrams to the destination, and reads only return datagrams
// the relay says came from it. Safe for one reader and one writer at a time,
// as the nat uses it.
type proxiedUdpConn struct {
	relayed     net.PacketConn
	destination *net.UDPAddr
}

// Read returns the next datagram from the destination. The loop skips
// datagrams from anywhere else and ends with the read deadline or the
// association, like socks5PacketConn.ReadFrom.
func (self *proxiedUdpConn) Read(b []byte) (int, error) {
	for {
		n, from, err := self.relayed.ReadFrom(b)
		if err != nil {
			return 0, err
		}
		source, ok := from.(*net.UDPAddr)
		if !ok || source.Port != self.destination.Port || !source.IP.Equal(self.destination.IP) {
			// another destination's datagram on this association, which a
			// connected udp socket would not deliver either
			continue
		}
		return n, nil
	}
}

// Write sends one datagram to the destination through the relay.
func (self *proxiedUdpConn) Write(b []byte) (int, error) {
	return self.relayed.WriteTo(b, self.destination)
}

// Close closes the relay socket and the association's control connection.
func (self *proxiedUdpConn) Close() error {
	return self.relayed.Close()
}

func (self *proxiedUdpConn) LocalAddr() net.Addr           { return self.relayed.LocalAddr() }
func (self *proxiedUdpConn) RemoteAddr() net.Addr          { return self.destination }
func (self *proxiedUdpConn) SetDeadline(t time.Time) error { return self.relayed.SetDeadline(t) }
func (self *proxiedUdpConn) SetReadDeadline(t time.Time) error {
	return self.relayed.SetReadDeadline(t)
}
func (self *proxiedUdpConn) SetWriteDeadline(t time.Time) error {
	return self.relayed.SetWriteDeadline(t)
}

// proxiedUdpRefusalReason is why a proxied identity refused a client udp flow.
type proxiedUdpRefusalReason int

const (
	proxiedUdpRefusalNoRelay proxiedUdpRefusalReason = iota
	proxiedUdpRefusalExitMismatch
	proxiedUdpRefusalAssociation
	proxiedUdpRefusalIpv6
)

func (self proxiedUdpRefusalReason) String() string {
	switch self {
	case proxiedUdpRefusalNoRelay:
		return "no-relay"
	case proxiedUdpRefusalExitMismatch:
		return "exit-mismatch"
	case proxiedUdpRefusalIpv6:
		return "ipv6 destination"
	default:
		return "association failed"
	}
}

const (
	// one line per identity and reason per interval, so a client that retries
	// a refused flow does not flood the log
	proxiedUdpRefusalLogInterval = 10 * time.Minute
	// the stats forget every identity once they hold this many, so they stay
	// bounded on a host with churning proxy lists
	proxiedUdpRefusalMaxKeys = 4096
)

// proxiedUdpRefusalKey is one identity and reason.
type proxiedUdpRefusalKey struct {
	proxyKey string
	reason   proxiedUdpRefusalReason
}

// proxiedUdpRefusal is the refusals of one identity and reason since its last
// log line.
type proxiedUdpRefusal struct {
	lastLogTime  time.Time
	refusedCount uint64
	totalCount   uint64
}

// proxiedUdpRefusalStats counts the client udp flows each proxied identity
// refused, by reason, and logs them throttled per identity and reason. Safe
// for concurrent use.
type proxiedUdpRefusalStats struct {
	stateLock   sync.Mutex
	keyRefusals map[proxiedUdpRefusalKey]*proxiedUdpRefusal
}

var proxiedUdpRefusals = &proxiedUdpRefusalStats{
	keyRefusals: map[proxiedUdpRefusalKey]*proxiedUdpRefusal{},
}

// note counts one refused flow and logs it when the identity's interval for
// the reason has passed, with the count since its previous line.
func (self *proxiedUdpRefusalStats) note(proxySettings *connect.ProxySettings, reason proxiedUdpRefusalReason) {
	key := proxiedUdpRefusalKey{proxyKey: proxySettings.Key(), reason: reason}
	now := time.Now()
	logCount := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		refusal := self.keyRefusals[key]
		if refusal == nil {
			if proxiedUdpRefusalMaxKeys <= len(self.keyRefusals) {
				clear(self.keyRefusals)
			}
			refusal = &proxiedUdpRefusal{}
			self.keyRefusals[key] = refusal
		}
		refusal.refusedCount += 1
		refusal.totalCount += 1
		if !refusal.lastLogTime.IsZero() && now.Sub(refusal.lastLogTime) < proxiedUdpRefusalLogInterval {
			return 0
		}
		refusal.lastLogTime = now
		count := refusal.refusedCount
		refusal.refusedCount = 0
		return count
	}()
	if 0 < logCount {
		tlog("[proxy][udp] proxy[%d] (%s) client udp refused: %s (%d since the last line for this proxy)\n", proxySettings.Index, redactedProxyAddress(proxySettings.Address), reason, logCount)
	}
}

// count is how many flows of an identity were refused for reason.
func (self *proxiedUdpRefusalStats) count(proxySettings *connect.ProxySettings, reason proxiedUdpRefusalReason) uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if refusal := self.keyRefusals[proxiedUdpRefusalKey{proxyKey: proxySettings.Key(), reason: reason}]; refusal != nil {
		return refusal.totalCount
	}
	return 0
}
