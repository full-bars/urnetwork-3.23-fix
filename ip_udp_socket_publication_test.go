package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

type udpPublicationBackend struct {
	sequence  *UdpSequence
	published bool
}

func (b *udpPublicationBackend) add(int) error {
	b.published = b.sequence.sharedSocket != nil
	return nil
}
func (b *udpPublicationBackend) remove(int)                             {}
func (b *udpPublicationBackend) wait([]udpSocketPollEvent) (int, error) { return 0, nil }
func (b *udpPublicationBackend) close() error                           { return nil }

func TestUdpSharedSocketPublishedBeforeRegistration(t *testing.T) {
	settings := testUdpSharedLifecycleSettings()
	sequence := NewUdpSequence(context.Background(), nil, DestinationId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(127, 0, 0, 1), 1, net.IPv4(127, 0, 0, 1), 9, settings)
	backend := &udpPublicationBackend{sequence: sequence}
	sequence.socketReadPoller = &udpSocketReadPoller{shards: []udpSocketReadPollShard{{
		backend: backend, byFd: make(map[int]*udpSocketReadRegistration),
	}}}
	sequence.sharedSocketLifecycle = true
	defer sequence.Close()
	if !sequence.startSharedSocket() {
		t.Fatal("socket registration failed")
	}
	if !backend.published {
		t.Fatal("socket was not published when registration became visible")
	}
}
