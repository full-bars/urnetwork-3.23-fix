package connect

import (
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// Round-trips the new sender_generation_id field through actual proto
// marshal/unmarshal. A hand-patched .pb.go struct field would compile but
// silently not appear on the wire, since protobuf-go serializes via the
// compiled descriptor (file_transfer_proto_rawDesc), not just struct tags —
// this test would fail in exactly that case.
func TestExchangeSignalsSenderGenerationIdRoundTrip(t *testing.T) {
	genId := []byte{0x01, 0x02, 0x03, 0x04}
	original := &protocol.ExchangeSignals{
		StreamId:           []byte{0xaa, 0xbb},
		ResetSignals:       true,
		SenderGenerationId: genId,
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, genId, decoded.SenderGenerationId)
	assert.Equal(t, original.StreamId, decoded.StreamId)
	assert.Equal(t, original.ResetSignals, decoded.ResetSignals)
}

// A peer that doesn't set sender_generation_id (the legacy/older-peer case)
// must round-trip cleanly with an empty field, not fail or panic.
func TestExchangeSignalsWithoutSenderGenerationId(t *testing.T) {
	original := &protocol.ExchangeSignals{
		StreamId: []byte{0xaa, 0xbb},
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, 0, len(decoded.SenderGenerationId))
}

// Confirms the previously drifted NetworkPeer/NetworkPeersReset/
// NetworkPeersUpdate messages (reconciled into transfer.proto/frame.proto in
// this same change) still round-trip correctly after the regen.
func TestNetworkPeerRoundTrip(t *testing.T) {
	disconnectTime := uint64(1234567890)
	original := &protocol.NetworkPeer{
		ClientId:       []byte{0x01},
		ProvideModes:   []protocol.ProvideMode{protocol.ProvideMode_Network},
		Principal:      "test-principal",
		Roles:          []string{"role-a", "role-b"},
		DeviceName:     "test-device",
		DeviceSpec:     "test-spec",
		DisconnectTime: &disconnectTime,
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.NetworkPeer{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, original.ClientId, decoded.ClientId)
	assert.Equal(t, original.ProvideModes, decoded.ProvideModes)
	assert.Equal(t, original.Principal, decoded.Principal)
	assert.Equal(t, original.Roles, decoded.Roles)
	assert.Equal(t, original.DeviceName, decoded.DeviceName)
	assert.Equal(t, original.DeviceSpec, decoded.DeviceSpec)
	assert.Equal(t, *original.DisconnectTime, decoded.GetDisconnectTime())
}

func TestNetworkPeersUpdateRoundTrip(t *testing.T) {
	original := &protocol.NetworkPeersUpdate{
		Peers: []*protocol.NetworkPeer{
			{ClientId: []byte{0x01}, Principal: "a"},
			{ClientId: []byte{0x02}, Principal: "b"},
		},
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.NetworkPeersUpdate{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, 2, len(decoded.Peers))
	assert.Equal(t, original.Peers[0].ClientId, decoded.Peers[0].ClientId)
	assert.Equal(t, original.Peers[1].Principal, decoded.Peers[1].Principal)
}

func TestFrameMessageTypeNetworkPeersValues(t *testing.T) {
	// Pins the enum values (26/27) that were previously only present in the
	// compiled .pb.go and absent from frame.proto's source.
	assert.Equal(t, protocol.MessageType(26), protocol.MessageType_TransferNetworkPeersReset)
	assert.Equal(t, protocol.MessageType(27), protocol.MessageType_TransferNetworkPeersUpdate)
}
