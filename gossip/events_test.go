package gossip

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsubpb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

func TestEventRingWrapAndBounds(t *testing.T) {
	var r EventRing
	for i := 0; i < EventCapacity+10; i++ {
		if !r.Record(Event{Time: time.Unix(int64(i), 0), Kind: strings.Repeat("k", 200), Peer: strings.Repeat("p", 200), Direction: strings.Repeat("d", 200), Reason: strings.Repeat("r", 200)}) {
			t.Fatal("uncontended record dropped")
		}
	}
	events := r.Snapshot()
	if len(events) != EventCapacity || events[0].Time.Unix() != 10 || events[len(events)-1].Time.Unix() != EventCapacity+9 {
		t.Fatalf("unexpected ring contents: length=%d first=%v last=%v", len(events), events[0].Time, events[len(events)-1].Time)
	}
	e := events[0]
	if len(e.Kind) != 16 || len(e.Peer) != 8 || len(e.Direction) != 16 || len(e.Reason) != 64 {
		t.Fatalf("fields not bounded: %+v", e)
	}
	events[0].Kind = "changed"
	if r.Snapshot()[0].Kind == "changed" {
		t.Fatal("snapshot aliases ring")
	}
}

func TestEventRingDropsWhenBusy(t *testing.T) {
	var r EventRing
	r.mu.Lock()
	if r.Record(Event{Kind: "peer"}) {
		t.Fatal("busy ring accepted an event")
	}
	r.mu.Unlock()
	if len(r.Snapshot()) != 0 {
		t.Fatal("busy ring changed")
	}
}

func TestEventRingConcurrent(t *testing.T) {
	var r EventRing
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.Record(Event{Kind: "peer"})
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if len(r.Snapshot()) > EventCapacity {
		t.Fatal("ring exceeded its capacity")
	}
}

type fakeNetworkConn struct {
	network.Conn
	peer peer.ID
	dir  network.Direction
}

func (f *fakeNetworkConn) RemotePeer() peer.ID {
	return f.peer
}

func (f *fakeNetworkConn) Stat() network.ConnStats {
	return network.ConnStats{Stats: network.Stats{Direction: f.dir}}
}

func TestNodePeerEventsAndDialFailureClasses(t *testing.T) {
	clock := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	node := &Node{
		settings: &NodeSettings{
			Now: func() time.Time { return clock },
		},
	}

	peerIn, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	peerOut, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	peerDial, err := PeerIdForExtenderPublicKey(newTestKey(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}

	node.recordPeerEvent(&fakeNetworkConn{peer: peerIn, dir: network.DirInbound}, "connect")
	clock = clock.Add(time.Second)
	node.recordPeerEvent(&fakeNetworkConn{peer: peerOut, dir: network.DirOutbound}, "disconnect")

	for _, err := range []error{
		context.DeadlineExceeded,
		syscall.ECONNREFUSED,
		context.Canceled,
		errors.New("handshake failed"),
	} {
		clock = clock.Add(time.Second)
		class := node.dialStats.recordResult(err)
		node.eventRing().Record(Event{
			Time:      node.now(),
			Kind:      "dial",
			Peer:      peerDial.String(),
			Direction: "out",
			Reason:    class,
		})
	}

	events := node.Events()
	if len(events) != 6 {
		t.Fatalf("events count = %d, want 6", len(events))
	}
	if events[0].Kind != "peer" || events[0].Peer != peerIn.String()[:8] || events[0].Direction != "in" || events[0].Reason != "connect" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[1].Kind != "peer" || events[1].Peer != peerOut.String()[:8] || events[1].Direction != "out" || events[1].Reason != "disconnect" {
		t.Fatalf("event[1] = %+v", events[1])
	}
	wantReasons := []string{"timeout", "refused", "canceled", "other"}
	for i, want := range wantReasons {
		got := events[2+i]
		if got.Kind != "dial" || got.Peer != peerDial.String()[:8] || got.Direction != "out" || got.Reason != want {
			t.Fatalf("dial event[%d] = %+v, want reason %q", i, got, want)
		}
	}
}

func TestValidatorRejectSamplingAndFloodBounds(t *testing.T) {
	rootKey := newTestKey(t)
	otherRootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)

	clock := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	node := &Node{
		settings: &NodeSettings{
			NetworkHost:       testNetworkHost,
			Directory:         directory,
			RejectSampleEvery: 10,
			Now:               func() time.Time { return clock },
		},
	}

	senderPeer, err := PeerIdForExtenderPublicKey(extenderKey.publicKey)
	if err != nil {
		t.Fatal(err)
	}

	// 1st reject (n=1) is sampled: malformed payload.
	malformed := []byte("secret-payload-that-must-not-appear")
	res := node.validate(context.Background(), senderPeer, &pubsub.Message{
		Message: &pubsubpb.Message{Data: malformed},
	})
	if res != pubsub.ValidationReject {
		t.Fatalf("expected reject, got %v", res)
	}
	events := node.Events()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != "reject" || events[0].Reason != "malformed" || events[0].Size != len(malformed) || events[0].Peer != senderPeer.String()[:8] {
		t.Fatalf("first reject event = %+v", events[0])
	}
	if strings.Contains(events[0].Reason, "secret-payload") {
		t.Fatalf("reject event leaked payload: %+v", events[0])
	}

	// Next 9 rejects (n=2..10) are suppressed; n=11 is sampled (signature failure).
	badSigRecord := signTestRecord(t, otherRootKey, extenderKey, "192.0.2.55", 8443, clock)
	badSigBytes, err := proto.Marshal(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: badSigRecord},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		clock = clock.Add(time.Millisecond)
		if r := node.validate(context.Background(), peer.ID("12D3KooWSigPeer"), &pubsub.Message{
			Message: &pubsubpb.Message{Data: badSigBytes},
		}); r != pubsub.ValidationReject {
			t.Fatalf("expected reject at %d, got %v", i, r)
		}
	}
	events = node.Events()
	if len(events) != 2 {
		t.Fatalf("events after 11 rejects = %d, want 2", len(events))
	}
	if events[1].Reason != "signature" || events[1].Size != len(badSigBytes) {
		t.Fatalf("second sampled reject = %+v", events[1])
	}

	// Flood of 10,000 rejected messages stays bounded by EventCapacity.
	for i := 0; i < 10000; i++ {
		clock = clock.Add(time.Millisecond)
		node.validate(context.Background(), peer.ID("12D3KooWFloodPeer"), &pubsub.Message{
			Message: &pubsubpb.Message{Data: malformed},
		})
	}
	events = node.Events()
	if len(events) != EventCapacity {
		t.Fatalf("events after 10,000 flood = %d, want %d", len(events), EventCapacity)
	}
}

type failingResourceManager struct {
	network.NullResourceManager
}

func (failingResourceManager) OpenConnection(network.Direction, bool, ma.Multiaddr) (network.ConnManagementScope, error) {
	return nil, errors.New("resource limit exceeded")
}

func TestCapEventsAcrossFeedListenerAndTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var ring EventRing
	clock := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }

	// 1. FeedServer subscriber cap hit.
	rootKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	feedSettings := DefaultFeedServerSettings()
	feedSettings.MaxSubscriberCount = 1
	feedSettings.KeepaliveTimeout = time.Minute
	feedSettings.Events = &ring
	feedSettings.Now = now
	feed := NewFeedServer(ctx, directory, nil, feedSettings)
	t.Cleanup(feed.Close)

	first := newTestFeedClient(t, feed, true)
	first.readEndOfSample(t)
	second := newTestFeedClient(t, feed, true)
	second.readEndOfSample(t)
	<-second.done

	// 2. InProcessListener accept queue full cap hit.
	clock = clock.Add(time.Second)
	listener := NewInProcessListener(ctx, &InProcessListenerSettings{
		AcceptQueueCount: 1,
		Events:           &ring,
		Now:              now,
	})
	t.Cleanup(listener.Close)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	listener.conns <- &inProcessConn{Conn: c1, released: make(chan struct{})}
	listener.Handle(c2)

	// 3. extenderTransport resource limit cap hit.
	clock = clock.Add(time.Second)
	extenderKey := newTestKey(t)
	applyTestFeedRecord(t, directory, rootKey, extenderKey, "198.51.100.44")
	peerId, err := PeerIdForExtenderPublicKey(extenderKey.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	raddr, err := ma.NewMultiaddr("/ip4/198.51.100.44/tcp/8443")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := newExtenderTransport(&extenderTransportSettings{
		Directory:       directory,
		ResourceManager: &failingResourceManager{},
		Events:          &ring,
		Now:             now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Dial(ctx, raddr, peerId); err == nil {
		t.Fatal("expected resource limit error")
	}

	events := ring.Snapshot()
	if len(events) != 3 {
		t.Fatalf("cap events = %+v, want 3", events)
	}
	if events[0].Kind != "cap" || events[0].Reason != "subscriber_cap" || events[0].Direction != "in" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[1].Kind != "cap" || events[1].Reason != "accept_queue_full" || events[1].Direction != "in" {
		t.Fatalf("event[1] = %+v", events[1])
	}
	if events[2].Kind != "cap" || events[2].Reason != "resource_limit" || events[2].Direction != "out" || events[2].Peer != peerId.String()[:8] {
		t.Fatalf("event[2] = %+v", events[2])
	}
}
