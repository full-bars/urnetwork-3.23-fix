package connect

// Stage 3 adapter: the small set of upstream symbols that transport.go and the
// framing layer reference but whose owning upstream files (transfer.go,
// transfer_route_manager.go, message_pool.go) are not swapped in this tree. Each
// is either verbatim from upstream or a documented no-op. Nothing here changes
// fork transfer behavior.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// The upstream H1 ACK priority lane is a transfer.go send-path feature. This tree
// has no consumer of the registry, so registering a companion route is a no-op and
// ACKs ride the ordinary platform send route, exactly as before the swap.
func registerH1AckPriorityRoute(route Route, priorityRoute Route) {}

func unregisterH1AckPriorityRoute(route Route) {}

// receiveAckHandoffResult is the outcome label type used by transfer progress
// tracing. Upstream transfer.go produces it; this tree never does.
type receiveAckHandoffResult uint8

const (
	receiveAckHandoffAccepted receiveAckHandoffResult = iota
	receiveAckHandoffAcceptedAfterWait
	receiveAckHandoffQueueFull
	receiveAckHandoffQueueWaitTimeout
	receiveAckHandoffSequenceMissing
	receiveAckHandoffSequenceClosed
)

// ErrSendPackNotAdmitted completes lifecycle observation when bounded admission
// returns false without a more specific error (named by transfer progress tracing).
var ErrSendPackNotAdmitted = errors.New("send Pack was not admitted")

var ErrMessageTooLarge = errors.New("message exceeds byte limit")

// MessagePoolReadAllLimit preserves MessagePoolReadAll's ownership contract
// while putting a hard ceiling on data read from an untrusted framed stream.
// On an error it returns no buffer, so callers never need to return a partial
// message to the pool.
func MessagePoolReadAllLimit(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("invalid message byte limit %d", maxBytes)
	}

	message, err := MessagePoolReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if maxBytes < int64(len(message)) {
		MessagePoolReturn(message)
		return nil, fmt.Errorf("%w: limit %d", ErrMessageTooLarge, maxBytes)
	}
	return message, nil
}

type typedTransport interface {
	TransportType() TransportType
}

func transportTypeOf(transport Transport) TransportType {
	if typed, ok := transport.(typedTransport); ok {
		if transportType := typed.TransportType(); transportType != "" {
			return transportType
		}
	}
	return TransportTypeUnknown
}

// receiveLaneTransport gives one physical receive lane an independent route
// identity while preserving the base transport's matching and priority rules.
// Hybrid H3 and P2P connections use siblings so immutable route properties can
// carry exact lane reliability without changing Route's allocation-free byte
// channel contract.
type receiveLaneTransport struct {
	transportId Id
	base        Transport
}

func newReceiveLaneTransport(base Transport) *receiveLaneTransport {
	return &receiveLaneTransport{transportId: NewId(), base: base}
}

func (self *receiveLaneTransport) TransportId() Id { return self.transportId }
func (self *receiveLaneTransport) TransportType() TransportType {
	return transportTypeOf(self.base)
}
func (self *receiveLaneTransport) Priority() int   { return self.base.Priority() }
func (self *receiveLaneTransport) Weight() float32 { return self.base.Weight() }
func (self *receiveLaneTransport) CanEvalRouteWeight(
	stats *RouteStats,
	remainingStats map[Transport]*RouteStats,
) bool {
	return self.base.CanEvalRouteWeight(stats, remainingStats)
}
func (self *receiveLaneTransport) RouteWeight(
	stats *RouteStats,
	remainingStats map[Transport]*RouteStats,
) float32 {
	return self.base.RouteWeight(stats, remainingStats)
}
func (self *receiveLaneTransport) MatchesSend(destination TransferPath) bool {
	return false
}
func (self *receiveLaneTransport) MatchesReceive(destination TransferPath) bool {
	return self.base.MatchesReceive(destination)
}
func (self *receiveLaneTransport) Downgrade(source TransferPath) {
	self.base.Downgrade(source)
}

// NewExtenderPacketDialContext returns a dial that yields a net.PacketConn
// reaching (destinationHost, destinationPort) through the extender.
//
// This is the h3 path. An extender relays one reliable byte stream and cannot
// see inside the inner tls, so it cannot reframe a quic stream into anything;
// the datagrams are framed explicitly instead and the extender turns them back
// into udp at the far end (net_extender_datagram.go).
func NewExtenderPacketDialContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialPacketContextFunction {
	extenderTlsConfig := newExtenderTlsConfig(extenderConfig)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.PacketConn, error) {
		switch network {
		case "udp", "udp4", "udp6":
		default:
			return nil, fmt.Errorf("extender packet dial supports udp, not %s", network)
		}

		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}

		serverConn, _, err := dialExtenderStream(
			ctx,
			connectSettings,
			extenderConfig,
			&ExtenderDial{
				DestinationHost: host,
				DestinationPort: port,
				Datagram:        true,
			},
			extenderTlsConfig,
		)
		if err != nil {
			return nil, err
		}

		// ReadFrom's address is informational: the extender already owns the
		// destination from the accepted header. Construct it without resolving
		// the hostname here; an unbound resolver would bypass the egress path
		// and can recurse into the tunnel on Windows.
		remote := newExtenderDatagramAddr(network, address)
		return newExtenderPacketConn(serverConn, remote), nil
	}
}
