// Exercises historical auth reconstruction through the current h3 handshake,
// including strict modern echoes, raw fallback, refusal, and joined ownership.
package connect

// Fork note: upstream also carries a PlatformTransport h3 handshake harness
// (h3AuthCompatibilityCase, testPlatformH3AuthCompatibility and four
// TestPlatformTransportH3Auth* cases). It needs ClientStrategySettings.
// ExtenderConfigs, RouteManager.HasActiveTransport and the upstream transport
// generation, none of which this tree has, so only the protocol-level decisions
// are taken here. The three assertMessagePoolOwnership calls are dropped for the
// same reason: the message-pool ownership harness is upstream-only.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Reconstructs Auth from 847460bba8aa31fb2e86ce007e9d5443d884544b, whose
// fields ended at provide_intent (7), before metadata (8) and lease version (9).
func legacyH3AuthDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	legacy := protodesc.ToDescriptorProto((&protocol.Auth{}).ProtoReflect().Descriptor())
	legacyFields := make([]*descriptorpb.FieldDescriptorProto, 0, 7)
	for _, field := range legacy.Field {
		if field.GetNumber() <= 7 {
			legacyFields = append(legacyFields, field)
		}
	}
	if len(legacyFields) != 7 {
		t.Fatalf("legacy auth field count = %d, want 7", len(legacyFields))
	}
	legacy.Field = legacyFields
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("h3_legacy_auth.proto"),
		Package:     proto.String("h3_legacy"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{legacy},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file.Messages().Get(0)
}

// Borrows the current framed request and decodes it with the old descriptor.
// The current Go container below exposes only those old known fields to the
// historical helper; appended fields remain unknown exactly as on the old peer.
func decodeLegacyH3Auth(descriptor protoreflect.MessageDescriptor, frameBytes []byte) (*protocol.Auth, error) {
	frame := &protocol.Frame{}
	if err := proto.Unmarshal(frameBytes, frame); err != nil {
		return nil, err
	}
	if frame.MessageType != protocol.MessageType_TransferAuth {
		return nil, fmt.Errorf("legacy auth frame type = %s", frame.MessageType)
	}
	legacy := dynamicpb.NewMessage(descriptor)
	if err := proto.Unmarshal(frame.MessageBytes, legacy); err != nil {
		return nil, err
	}
	if len(legacy.GetUnknown()) == 0 {
		return nil, fmt.Errorf("current auth did not carry unknown session fields")
	}
	fields := descriptor.Fields()
	request := &protocol.Auth{
		ByJwt:                     legacy.Get(fields.ByName("by_jwt")).String(),
		AppVersion:                legacy.Get(fields.ByName("app_version")).String(),
		InstanceId:                bytes.Clone(legacy.Get(fields.ByName("instance_id")).Bytes()),
		H3DatagramVersion:         uint32(legacy.Get(fields.ByName("h3_datagram_version")).Uint()),
		H3DatagramAcceptedVersion: uint32(legacy.Get(fields.ByName("h3_datagram_accepted_version")).Uint()),
		IpFamily:                  int32(legacy.Get(fields.ByName("ip_family")).Int()),
		ProvideIntent:             legacy.Get(fields.ByName("provide_intent")).Bool(),
	}
	request.ProtoReflect().SetUnknown(bytes.Clone(legacy.GetUnknown()))
	return request, nil
}

// The actual response reconstruction from Connect
// 847460bba8aa31fb2e86ce007e9d5443d884544b:transport_h3_datagram.go:151.
// Only the helper name and the historical constant's spelling differ; pinning
// version 2 prevents a future production version change from rewriting history.
func acceptLegacyH3DatagramAuthOffer(
	auth *protocol.Auth,
	enabled bool,
	localQuicSupport bool,
	remoteQuicSupport bool,
) (*protocol.Auth, bool) {
	const legacyH3DatagramProtocolVersion uint32 = 2
	accepted := enabled && localQuicSupport && remoteQuicSupport &&
		auth.H3DatagramVersion == legacyH3DatagramProtocolVersion
	response := &protocol.Auth{
		ByJwt:             auth.ByJwt,
		AppVersion:        auth.AppVersion,
		InstanceId:        bytes.Clone(auth.InstanceId),
		H3DatagramVersion: auth.H3DatagramVersion,
	}
	if accepted {
		response.H3DatagramAcceptedVersion = legacyH3DatagramProtocolVersion
	}
	return response, accepted
}

// A wire-decoded old response negotiates the legacy carrier without mutating
// either auth message or changing the exported strict validator's contract.
func TestH3AuthCompatibilityAcceptsOnlyCompleteLegacyOmission(t *testing.T) {
	request := &protocol.Auth{
		ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
		InstanceId: NewId().Bytes(), ClientInfo: `{"v":1,"device_type":"synthetic"}`,
		StreamLeaseVersion: 1,
	}
	SetH3DatagramAuthOffer(request, true)
	requestBytes, err := EncodeFrame(request, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(requestBytes)
	legacyRequest, err := decodeLegacyH3Auth(legacyH3AuthDescriptor(t), requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse, accepted := acceptLegacyH3DatagramAuthOffer(legacyRequest, true, true, true)
	if !accepted || legacyResponse.ClientInfo != "" || legacyResponse.StreamLeaseVersion != 0 ||
		len(legacyResponse.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("historical helper did not reconstruct its accepted response")
	}
	responseBytes, err := EncodeFrame(legacyResponse, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(responseBytes)
	decoded, err := DecodeFrame(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	response := decoded.(*protocol.Auth)
	requestBefore, responseBefore := proto.Clone(request), proto.Clone(response)
	if accepted, err := ValidateH3DatagramAuthResponse(request, response, true, true, true); accepted || err == nil {
		t.Fatalf("strict validator accepted missing echoes: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, response, true, true, true); !accepted || err != nil {
		t.Fatalf("historical accepted response failed negotiation: (%t, %v)", accepted, err)
	}
	if !proto.Equal(request, requestBefore) || !proto.Equal(response, responseBefore) {
		t.Fatal("negotiation mutated borrowed auth messages")
	}
	modernResponse, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
	if accepted, err := negotiateH3DatagramAuthResponse(request, modernResponse, true, true, true); !accepted || err != nil {
		t.Fatalf("modern response failed negotiation: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, request, true, true, false); accepted || err != nil {
		t.Fatalf("raw echo failed reliable fallback: (%t, %v)", accepted, err)
	}
}

// Both response generations retain identity, offer, version and transport
// capability checks. Partial or contradictory session echoes cannot downgrade.
func TestH3AuthCompatibilityRejectsIdentityAndCapabilityChanges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, c := range []struct {
			name   string
			change func(*protocol.Auth, *protocol.Auth)
		}{
			{name: "credential", change: func(_, response *protocol.Auth) { response.ByJwt += "-other" }},
			{name: "app version", change: func(_, response *protocol.Auth) { response.AppVersion += "-other" }},
			{name: "instance", change: func(_, response *protocol.Auth) { response.InstanceId[0] ^= 1 }},
			{name: "offer", change: func(_, response *protocol.Auth) { response.H3DatagramVersion++ }},
			{name: "unoffered", change: func(request, _ *protocol.Auth) { request.H3DatagramVersion = 0 }},
			{name: "unknown accepted", change: func(_, response *protocol.Auth) { response.H3DatagramAcceptedVersion++ }},
			{name: "contradictory metadata", change: func(_, response *protocol.Auth) { response.ClientInfo = `{"v":1,"device_type":"other"}` }},
			{name: "contradictory lease", change: func(_, response *protocol.Auth) { response.StreamLeaseVersion = 2 }},
			{name: "metadata only", change: func(request, response *protocol.Auth) {
				response.ClientInfo = request.ClientInfo
				response.StreamLeaseVersion = 0
			}},
			{name: "lease only", change: func(_, response *protocol.Auth) { response.ClientInfo = ""; response.StreamLeaseVersion = 1 }},
			{name: "future lease offer", change: func(request, _ *protocol.Auth) { request.StreamLeaseVersion = 2 }},
		} {
			request := &protocol.Auth{
				ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
				InstanceId: NewId().Bytes(), ClientInfo: `{"v":1}`, StreamLeaseVersion: 1,
			}
			SetH3DatagramAuthOffer(request, true)
			response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
			if legacy {
				response, _ = acceptLegacyH3DatagramAuthOffer(request, true, true, true)
			}
			c.change(request, response)
			if accepted, err := negotiateH3DatagramAuthResponse(request, response, true, true, true); accepted || err == nil {
				t.Fatalf("legacy=%t %s accepted: (%t, %v)", legacy, c.name, accepted, err)
			}
		}
	}
	request := &protocol.Auth{
		ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
		InstanceId: NewId().Bytes(), ClientInfo: `{"v":1}`, StreamLeaseVersion: 1,
	}
	SetH3DatagramAuthOffer(request, true)
	for _, legacy := range []bool{false, true} {
		response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
		if legacy {
			response, _ = acceptLegacyH3DatagramAuthOffer(request, true, true, true)
		}
		for _, capabilities := range [][3]bool{{false, true, true}, {true, false, true}, {true, true, false}} {
			if accepted, err := negotiateH3DatagramAuthResponse(request, response, capabilities[0], capabilities[1], capabilities[2]); accepted || err == nil {
				t.Fatalf("legacy=%t capabilities=%v accepted: (%t, %v)", legacy, capabilities, accepted, err)
			}
		}
	}
	legacyResponse, _ := acceptLegacyH3DatagramAuthOffer(request, true, true, true)
	legacyResponse.H3DatagramAcceptedVersion = 0
	if accepted, err := negotiateH3DatagramAuthResponse(request, legacyResponse, true, true, true); accepted || err == nil {
		t.Fatalf("omitted echoes without acceptance selected fallback: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, nil, true, true, true); accepted || err == nil {
		t.Fatalf("nil response accepted: (%t, %v)", accepted, err)
	}
}

// Describes the server's response at the real stream handshake boundary.
