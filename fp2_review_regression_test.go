package connect

import (
	"context"
	"crypto/tls"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestFP2InitialWindowAllowDirectOverride(t *testing.T) {
	for _, allow := range []bool{false, true} {
		settings := DefaultMultiClientSettings()
		settings.DefaultPerformanceProfile = &PerformanceProfile{AllowDirect: !allow}
		settings.OverrideAllowDirect = &allow
		client := NewRemoteUserNatMultiClient(context.Background(), &testingEmptyMultiClientGenerator{}, nil, protocol.ProvideMode_Network, settings)
		for kind, window := range client.windows {
			window.stateLock.Lock()
			got := window.performanceProfile.AllowDirect
			window.stateLock.Unlock()
			if got != allow {
				t.Errorf("window %v: AllowDirect = %v, want %v", kind, got, allow)
			}
		}
		client.Close()
	}
}

func TestFP2CompanionSealPreservesOrdinarySession(t *testing.T) {
	r := NewPeerCapabilityRecorder(1)
	id := NewId()
	ordinary := &peerEncryptionSession{}
	companion := &peerEncryptionSession{companion: true}
	r.observeE2eSealed(id, tls.X25519, ordinary)
	r.observeE2eSealed(id, tls.X25519MLKEM768, companion)
	c := r.peerIdCapabilities[id]
	if c.sealedSession.Value() != ordinary || !c.e2eSealed || c.keyExchange != tls.X25519MLKEM768 {
		t.Fatal("companion seal lost ordinary session or capability evidence")
	}
	runtime.KeepAlive(ordinary)
	runtime.KeepAlive(companion)
}

func TestFP2ReceiveWindowZeroRecordedOnce(t *testing.T) {
	r := NewPeerCapabilityRecorder(1)
	calls := 0
	r.nowForTest = func() time.Time { calls++; return time.Unix(int64(calls), 0) }
	s := &SendSequence{client: &Client{peerCapabilityRecorder: r}, destination: DestinationId(NewId())}
	zero, one := uint64(0), uint64(1)
	s.observePeerCapabilityAck(&protocol.Ack{})
	s.observePeerCapabilityAck(&protocol.Ack{ReceiveWindowByteCount: &zero})
	s.observePeerCapabilityAck(&protocol.Ack{ReceiveWindowByteCount: &zero})
	s.observePeerCapabilityAck(&protocol.Ack{ReceiveWindowByteCount: &one})
	s.observePeerCapabilityAck(&protocol.Ack{ReceiveWindowByteCount: &one})
	if calls != 3 {
		t.Fatalf("recorded %d observations, want ack, first zero and increase", calls)
	}
}

func TestFP2ReceiveWindowClamp(t *testing.T) {
	r := NewPeerCapabilityRecorder(1)
	id := NewId()
	s := &SendSequence{client: &Client{peerCapabilityRecorder: r}, destination: DestinationId(id)}
	for _, window := range []uint64{1, math.MaxUint32, uint64(math.MaxUint32) + 1, math.MaxUint64} {
		s.observePeerCapabilityAck(&protocol.Ack{ReceiveWindowByteCount: &window})
		want := min(window, uint64(math.MaxUint32))
		if got := s.peerReceiveWindowMax.Load(); got != want {
			t.Fatalf("window %d: cache = %d, want %d", window, got, want)
		}
		got, ok := r.Get(id)
		if !ok || got.MaxReceiveWindowByteCount != ByteCount(want) {
			t.Fatalf("window %d: observation = %+v", window, got)
		}
	}
}
