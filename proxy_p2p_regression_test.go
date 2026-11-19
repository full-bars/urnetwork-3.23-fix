package connect

import (
	"context"
	"reflect"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestProxiedClientRefusesPeerStreams(t *testing.T) {
	settings := DefaultWebRtcSettings()
	field := reflect.ValueOf(settings).Elem().FieldByName("DisablePeerConnections")
	if field.IsValid() {
		field.SetBool(true)
	}
	rec := &transportRecorder{}
	buffer := newLifetimeTestBuffer(t, &StreamBufferSettings{P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
		transport, cancel := fakeChildTransport(sequence)
		rec.add(cancel)
		return []*P2pTransport{transport}
	}})
	manager := &StreamManager{streamBuffer: buffer, webRtcManager: NewWebRtcManager(context.Background(), nil, settings)}
	streamID := NewId()
	frame, err := ToFrame(&protocol.StreamOpen{DestinationId: NewId().Bytes(), StreamId: streamID.Bytes()}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.handleControlFrame(frame); err != nil {
		t.Fatal(err)
	}
	if manager.IsStreamOpen(streamID) {
		t.Fatal("proxied client accepted a peer stream")
	}
}

func TestDisabledPeerManagerRefusesWithoutConnection(t *testing.T) {
	settings := DefaultWebRtcSettings()
	settings.DisablePeerConnections = true
	manager := NewWebRtcManager(context.Background(), nil, settings)
	if conn, err := manager.NewP2pConnActive(context.Background(), TransferPath{}); err != ErrPeerConnectionsDisabled || conn != nil {
		t.Fatalf("active connection: %v, %v", conn, err)
	}
	if conn, err := manager.NewP2pConnPassive(context.Background(), TransferPath{}); err != ErrPeerConnectionsDisabled || conn != nil {
		t.Fatalf("passive connection: %v, %v", conn, err)
	}
}
