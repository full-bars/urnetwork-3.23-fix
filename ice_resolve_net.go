//go:build !js

// Native WebRTC address resolution must follow the same egress and lifecycle
// rules as the sockets ICE creates. Pion transport.Net exposes only the
// context-free net.Resolve* shape, so its candidate gatherer otherwise cannot
// interrupt a DNS query when a peer generation is retired.
//
// Ported to urnetwork-3.23-fix from the v2026 engine,
// github.com/full-bars/connect@v0.0.0-20261017075543-c8d8cdf536a8
// (ice_resolve_net.go). The resolver itself is unchanged; the one adaptation is
// the constructor signature: newPeerConnectionResolveNet takes the peer
// generation's context. The newer generation hardcodes context.Background()
// here and relies on its separate peerConn.teardown() step calling the returned
// cancel, whereas this generation retires a peer by canceling the peer
// context. Both cancellations are live on the native path
// (transport_p2p_webrtc.go): binding the resolver to the generation's context
// ends a lookup the moment the generation is retired -- Pion gathers STUN/TURN
// hostnames in goroutines detached from SetLocalDescription, so a query can
// still be parked on a socket read when the peer goes away -- and Run's
// deferred teardown also calls the returned cancel before
// PeerConnection.Close joins the ICE gatherer, covering the paths that end Run
// without retiring its context.
package connect

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/pion/transport/v4"
)

// peerConnectionResolveNet gives one peer generation an independently
// cancelable resolver while preserving its underlying socket network.
type peerConnectionResolveNet struct {
	transport.Net
	ctx      context.Context
	resolver *net.Resolver
	timeout  time.Duration
	// lookupNetIP is a test seam for the address-order rule. The real resolver
	// sorts answers by RFC 6724, so a fake DNS server cannot make an
	// "AAAA first" answer survive to LookupNetIP deterministically; tests
	// replace this to drive the exact order the rule exists for.
	lookupNetIP func(ctx context.Context, network string, host string) ([]netip.Addr, error)
}

// newPeerConnectionResolveNet creates the per-generation cancellation owner.
// parentCtx is the peer generation's context, so retiring that generation
// cancels its name lookups even if the caller never reaches the explicit
// cancel. The caller must cancel the returned func before asking Pion to join
// candidate gathering.
func newPeerConnectionResolveNet(
	parentCtx context.Context,
	base transport.Net,
	resolver *net.Resolver,
	timeout time.Duration,
) (*peerConnectionResolveNet, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parentCtx)
	if resolver != nil && resolver.Dial != nil {
		// net.Resolver may detach a shared wire lookup from its caller's
		// cancellation. Bind each underlying control dial to this exact peer as
		// well, so the detached lookup cannot outlive the retired generation.
		baseDial := resolver.Dial
		resolver = &net.Resolver{
			PreferGo:     resolver.PreferGo,
			StrictErrors: resolver.StrictErrors,
		}
		resolver.Dial = func(dialCtx context.Context, network string, address string) (net.Conn, error) {
			operationCtx, cancelOperation := context.WithCancelCause(dialCtx)
			stop := context.AfterFunc(ctx, func() {
				cancelOperation(context.Cause(ctx))
			})
			if ctx.Err() != nil {
				cancelOperation(context.Cause(ctx))
			}
			defer stop()
			defer cancelOperation(context.Canceled)
			return baseDial(operationCtx, network, address)
		}
	}
	return &peerConnectionResolveNet{
		Net:      base,
		ctx:      ctx,
		resolver: resolver,
		timeout:  timeout,
	}, cancel
}

// operationContext applies both the peer lifetime and the configured STUN
// gathering bound to name resolution, which Pion's timeout does not cover.
func (self *peerConnectionResolveNet) operationContext() (
	context.Context,
	context.CancelFunc,
) {
	if 0 < self.timeout {
		return context.WithTimeout(self.ctx, self.timeout)
	}
	return context.WithCancel(self.ctx)
}

// resolverOrDefault retains ordinary platform DNS when no egress resolver is
// needed on this platform.
func (self *peerConnectionResolveNet) resolverOrDefault() *net.Resolver {
	if self.resolver != nil {
		return self.resolver
	}
	return net.DefaultResolver
}

// lookupAddresses applies the test seam when one is installed and otherwise
// resolves through this peer's resolver.
func (self *peerConnectionResolveNet) lookupAddresses(
	ctx context.Context,
	lookupNetwork string,
	host string,
) ([]netip.Addr, error) {
	if self.lookupNetIP != nil {
		return self.lookupNetIP(ctx, lookupNetwork, host)
	}
	return self.resolverOrDefault().LookupNetIP(ctx, lookupNetwork, host)
}

// ipNetwork maps a transport endpoint network onto net.Resolver's accepted
// LookupNetIP network names.
func ipNetwork(network string) (string, error) {
	switch network {
	case "ip", "udp", "tcp":
		return "ip", nil
	case "ip4", "udp4", "tcp4":
		return "ip4", nil
	case "ip6", "udp6", "tcp6":
		return "ip6", nil
	default:
		return "", net.UnknownNetworkError(network)
	}
}

// preferredAddress applies net.Resolve*'s preference for IPv4 when the network
// leaves the family open. LookupNetIP returns the resolver's order, and a
// dual-stack STUN/TURN name can put its AAAA first; joining that address parks
// candidate gathering on a dead or filtered v6 path. A family-filtered lookup
// is already ordered by the resolver and keeps its first answer.
func preferredAddress(addresses []netip.Addr, lookupNetwork string) netip.Addr {
	if lookupNetwork != "ip" {
		return addresses[0]
	}
	for _, address := range addresses {
		if address.Unmap().Is4() {
			return address
		}
	}
	return addresses[0]
}

// literalFamilyAllowed mirrors net.Resolve*'s family check for an address
// literal, which never reaches the resolver: ip4 accepts the IPv4 forms
// (including an IPv4-mapped IPv6 literal) and rejects the rest; ip6 accepts
// only a real IPv6 literal, so an IPv4-mapped one is refused exactly as
// net.ResolveUDPAddr refuses it; a family-agnostic network accepts either.
func literalFamilyAllowed(address netip.Addr, lookupNetwork string) bool {
	switch lookupNetwork {
	case "ip4":
		return address.Unmap().Is4()
	case "ip6":
		return !address.Unmap().Is4()
	default:
		return true
	}
}

// resolveIPAddr resolves one hostname with cancellation and preserves literal
// IPv6 zones without involving DNS.
func (self *peerConnectionResolveNet) resolveIPAddr(
	ctx context.Context,
	network string,
	host string,
) (*net.IPAddr, error) {
	lookupNetwork, err := ipNetwork(network)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return &net.IPAddr{}, nil
	}
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if !literalFamilyAllowed(address, lookupNetwork) {
			return nil, &net.AddrError{Err: "no suitable address found", Addr: host}
		}
		return &net.IPAddr{
			IP:   net.IP(address.AsSlice()),
			Zone: address.Zone(),
		}, nil
	}
	addresses, err := self.lookupAddresses(ctx, lookupNetwork, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	address := preferredAddress(addresses, lookupNetwork)
	return &net.IPAddr{
		IP:   net.IP(address.AsSlice()),
		Zone: address.Zone(),
	}, nil
}

// resolvePort resolves a numeric or named service within the same cancellation
// boundary as the hostname.
func (self *peerConnectionResolveNet) resolvePort(
	ctx context.Context,
	network string,
	port string,
) (int, error) {
	if number, err := strconv.Atoi(port); err == nil {
		if number < 0 || 65535 < number {
			return 0, fmt.Errorf("invalid port %q", port)
		}
		return number, nil
	}
	return self.resolverOrDefault().LookupPort(ctx, network, port)
}

// ResolveIPAddr is the cancellation-aware form used by ICE IP endpoints.
func (self *peerConnectionResolveNet) ResolveIPAddr(
	network string,
	address string,
) (*net.IPAddr, error) {
	ctx, cancel := self.operationContext()
	defer cancel()
	return self.resolveIPAddr(ctx, network, address)
}

// ResolveUDPAddr is the cancellation-aware form used by STUN and UDP TURN
// gathering.
func (self *peerConnectionResolveNet) ResolveUDPAddr(
	network string,
	address string,
) (*net.UDPAddr, error) {
	host, portValue, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := self.operationContext()
	defer cancel()
	ipAddress, err := self.resolveIPAddr(ctx, network, host)
	if err != nil {
		return nil, err
	}
	port, err := self.resolvePort(ctx, "udp", portValue)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{
		IP:   ipAddress.IP,
		Port: port,
		Zone: ipAddress.Zone,
	}, nil
}

// ResolveTCPAddr is the cancellation-aware adjacent path used by TCP and TLS
// TURN gathering.
func (self *peerConnectionResolveNet) ResolveTCPAddr(
	network string,
	address string,
) (*net.TCPAddr, error) {
	host, portValue, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := self.operationContext()
	defer cancel()
	ipAddress, err := self.resolveIPAddr(ctx, network, host)
	if err != nil {
		return nil, err
	}
	port, err := self.resolvePort(ctx, "tcp", portValue)
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{
		IP:   ipAddress.IP,
		Port: port,
		Zone: ipAddress.Zone,
	}, nil
}
