package connect

import (
	"net"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// Regression tests for findings 2.1/2.2 from the 2026-07-13 audit of PR #263
// (ip.go), verifying the fix in commit c4065b4: `StreamState.DataPackets`
// (UDP) and `ConnectionState.DataPackets` (TCP) both used to compute
// `packetByteCount := mtu - headerByteCount` unconditionally. With an MTU
// configured at or below the IP+transport header size, this went to zero or
// negative, causing a divide-by-zero panic in the chunk-count formula
// `(n+packetByteCount-1)/packetByteCount` or a negative slice-capacity panic
// in `make`. Both functions now return an error instead of panicking.

func newTestStreamState(ipVersion int) *StreamState {
	return &StreamState{
		ipVersion:       ipVersion,
		sourceIp:        net.ParseIP("10.0.0.1"),
		sourcePort:      1234,
		destinationIp:   net.ParseIP("10.0.0.2"),
		destinationPort: 5678,
		provideMode:     protocol.ProvideMode_Network,
	}
}

func newTestConnectionState(ipVersion int) *ConnectionState {
	return &ConnectionState{
		ipVersion:       ipVersion,
		sourceIp:        net.ParseIP("10.0.0.1"),
		sourcePort:      1234,
		destinationIp:   net.ParseIP("10.0.0.2"),
		destinationPort: 5678,
		provideMode:     protocol.ProvideMode_Network,
		receiveSeq:      1,
	}
}

func TestStreamStateDataPacketsRejectsUndersizedMtu(t *testing.T) {
	payload := make([]byte, 100)

	// IPv4+UDP header is 28 bytes; mtu == header and mtu < header must both
	// error rather than panic (divide-by-zero at mtu==header, negative
	// capacity/slice bounds at mtu<header).
	for _, mtu := range []int{0, 1, 20, 27, 28} {
		stream := newTestStreamState(4)
		packets, err := stream.DataPackets(payload, len(payload), mtu)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, 0, len(packets))
	}

	// IPv6+UDP header is 48 bytes.
	for _, mtu := range []int{0, 1, 40, 47, 48} {
		stream := newTestStreamState(6)
		packets, err := stream.DataPackets(payload, len(payload), mtu)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, 0, len(packets))
	}
}

func TestStreamStateDataPacketsAcceptsValidMtu(t *testing.T) {
	// Guard against an overcorrection: a valid MTU must still produce
	// packets, not spuriously error.
	stream := newTestStreamState(4)
	payload := make([]byte, 100)
	packets, err := stream.DataPackets(payload, len(payload), 29)
	assert.Equal(t, err, nil)
	assert.Equal(t, true, 0 < len(packets))

	// And still fragments correctly when the payload exceeds one packet's
	// worth of room.
	stream2 := newTestStreamState(4)
	big := make([]byte, 1000)
	packets2, err2 := stream2.DataPackets(big, len(big), 100)
	assert.Equal(t, err2, nil)
	assert.Equal(t, true, 1 < len(packets2))
}

func TestConnectionStateDataPacketsRejectsUndersizedMtu(t *testing.T) {
	payload := make([]byte, 100)

	// IPv4+TCP header is 40 bytes.
	for _, mtu := range []int{0, 1, 20, 39, 40} {
		conn := newTestConnectionState(4)
		packets, err := conn.DataPackets(payload, len(payload), mtu)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, 0, len(packets))
	}

	// IPv6+TCP header is 60 bytes.
	for _, mtu := range []int{0, 1, 40, 59, 60} {
		conn := newTestConnectionState(6)
		packets, err := conn.DataPackets(payload, len(payload), mtu)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, 0, len(packets))
	}
}

func TestConnectionStateDataPacketsAcceptsValidMtu(t *testing.T) {
	conn := newTestConnectionState(4)
	payload := make([]byte, 100)
	packets, err := conn.DataPackets(payload, len(payload), 41)
	assert.Equal(t, err, nil)
	assert.Equal(t, true, 0 < len(packets))

	conn2 := newTestConnectionState(4)
	big := make([]byte, 1000)
	packets2, err2 := conn2.DataPackets(big, len(big), 100)
	assert.Equal(t, err2, nil)
	assert.Equal(t, true, 1 < len(packets2))
}
