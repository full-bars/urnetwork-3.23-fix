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

// The enum name tables (MessageType_name/MessageType_value, embedded in the
// generated .pb.go and driven by the descriptor) must include the new
// values with the exact names declared in frame.proto.
func TestFrameMessageTypeNetworkPeersStringNames(t *testing.T) {
	assert.Equal(t, "TransferNetworkPeersReset", protocol.MessageType_TransferNetworkPeersReset.String())
	assert.Equal(t, "TransferNetworkPeersUpdate", protocol.MessageType_TransferNetworkPeersUpdate.String())
}

// A full ExchangeSignals with signals populated alongside the new
// sender_generation_id field must round-trip every field together, not just
// sender_generation_id in isolation.
func TestExchangeSignalsFullRoundTrip(t *testing.T) {
	genId := []byte{0xde, 0xad, 0xbe, 0xef}
	original := &protocol.ExchangeSignals{
		StreamId:     []byte{0x01, 0x02, 0x03},
		ResetSignals: false,
		Signals: []*protocol.ExchangeSignal{
			{
				SignalType: protocol.SignalType_SdpOffer,
				Sdp:        []byte(`{"type":"offer"}`),
			},
			{
				SignalType:   protocol.SignalType_IceCandidate,
				IceCandidate: []byte(`{"candidate":"..."}`),
			},
		},
		SenderGenerationId: genId,
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, original.StreamId, decoded.StreamId)
	assert.Equal(t, original.ResetSignals, decoded.ResetSignals)
	assert.Equal(t, genId, decoded.SenderGenerationId)
	assert.Equal(t, 2, len(decoded.Signals))
	assert.Equal(t, original.Signals[0].Sdp, decoded.Signals[0].Sdp)
	assert.Equal(t, original.Signals[1].IceCandidate, decoded.Signals[1].IceCandidate)
}

// The generated getter must be nil-safe, matching every other generated
// accessor in this package (`if x != nil { return x.Field }; return nil`).
func TestExchangeSignalsGetSenderGenerationIdNilSafety(t *testing.T) {
	var nilSignals *protocol.ExchangeSignals
	assert.Equal(t, []byte(nil), nilSignals.GetSenderGenerationId())

	populated := &protocol.ExchangeSignals{SenderGenerationId: []byte{0x01}}
	assert.Equal(t, []byte{0x01}, populated.GetSenderGenerationId())
}

// An empty sender_generation_id (explicitly zero-length, not nil) must
// still round-trip as empty, not be promoted to nil or vice versa causing
// spurious diffs downstream.
func TestExchangeSignalsEmptySenderGenerationId(t *testing.T) {
	original := &protocol.ExchangeSignals{
		StreamId:           []byte{0xaa},
		SenderGenerationId: []byte{},
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.ExchangeSignals{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, 0, len(decoded.SenderGenerationId))
}

// disconnect_time is `optional`, so an unset peer (the common, connected
// case) must decode with a nil pointer and GetDisconnectTime() must fall
// back to 0, not panic on a nil dereference.
func TestNetworkPeerWithoutDisconnectTime(t *testing.T) {
	original := &protocol.NetworkPeer{
		ClientId:  []byte{0x01},
		Principal: "connected-peer",
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.NetworkPeer{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	if decoded.DisconnectTime != nil {
		t.Fatalf("expected DisconnectTime to be nil, got %v", *decoded.DisconnectTime)
	}
	assert.Equal(t, uint64(0), decoded.GetDisconnectTime())
}

// provide_modes is a repeated enum; a peer with several enabled modes must
// preserve both the values and their order.
func TestNetworkPeerMultipleProvideModes(t *testing.T) {
	original := &protocol.NetworkPeer{
		ClientId: []byte{0x02},
		ProvideModes: []protocol.ProvideMode{
			protocol.ProvideMode_Network,
			protocol.ProvideMode_Public,
			protocol.ProvideMode_PublicStream,
		},
	}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.NetworkPeer{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, original.ProvideModes, decoded.ProvideModes)
}

// NetworkPeersReset carries no fields; it must still round-trip (as a
// zero-length payload) since it is used purely as a control signal.
func TestNetworkPeersResetRoundTrip(t *testing.T) {
	original := &protocol.NetworkPeersReset{}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)
	assert.Equal(t, 0, len(data))

	decoded := &protocol.NetworkPeersReset{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)
}

// An empty NetworkPeersUpdate (no peers) is a valid, if degenerate, upsert
// and must not error or produce a nil vs. empty-slice mismatch.
func TestNetworkPeersUpdateEmptyPeers(t *testing.T) {
	original := &protocol.NetworkPeersUpdate{}

	data, err := ProtoMarshal(original)
	assert.Equal(t, err, nil)

	decoded := &protocol.NetworkPeersUpdate{}
	err = ProtoUnmarshal(data, decoded)
	assert.Equal(t, err, nil)

	assert.Equal(t, 0, len(decoded.Peers))
}

// Exercises the documented usage pattern from frame.proto: a `Frame` whose
// `message_type` is `TransferNetworkPeersReset`/`TransferNetworkPeersUpdate`
// carries the corresponding marshaled control message in `message_bytes`.
func TestFrameCarryingNetworkPeersMessages(t *testing.T) {
	resetBytes, err := ProtoMarshal(&protocol.NetworkPeersReset{})
	assert.Equal(t, err, nil)

	resetFrame := &protocol.Frame{
		MessageType:  protocol.MessageType_TransferNetworkPeersReset,
		MessageBytes: resetBytes,
	}
	resetFrameData, err := ProtoMarshal(resetFrame)
	assert.Equal(t, err, nil)

	decodedResetFrame := &protocol.Frame{}
	err = ProtoUnmarshal(resetFrameData, decodedResetFrame)
	assert.Equal(t, err, nil)
	assert.Equal(t, protocol.MessageType_TransferNetworkPeersReset, decodedResetFrame.MessageType)

	decodedReset := &protocol.NetworkPeersReset{}
	err = ProtoUnmarshal(decodedResetFrame.MessageBytes, decodedReset)
	assert.Equal(t, err, nil)

	updateBytes, err := ProtoMarshal(&protocol.NetworkPeersUpdate{
		Peers: []*protocol.NetworkPeer{{ClientId: []byte{0x09}, Principal: "p"}},
	})
	assert.Equal(t, err, nil)

	updateFrame := &protocol.Frame{
		MessageType:  protocol.MessageType_TransferNetworkPeersUpdate,
		MessageBytes: updateBytes,
	}
	updateFrameData, err := ProtoMarshal(updateFrame)
	assert.Equal(t, err, nil)

	decodedUpdateFrame := &protocol.Frame{}
	err = ProtoUnmarshal(updateFrameData, decodedUpdateFrame)
	assert.Equal(t, err, nil)
	assert.Equal(t, protocol.MessageType_TransferNetworkPeersUpdate, decodedUpdateFrame.MessageType)

	decodedUpdate := &protocol.NetworkPeersUpdate{}
	err = ProtoUnmarshal(decodedUpdateFrame.MessageBytes, decodedUpdate)
	assert.Equal(t, err, nil)
	assert.Equal(t, 1, len(decodedUpdate.Peers))
	assert.Equal(t, "p", decodedUpdate.Peers[0].Principal)
}
