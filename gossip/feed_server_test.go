// The feed server (EXTENDER.md A8, D4).
//
// Every stream here is a pipe, because what the caps bound is the server's own
// bookkeeping and a pipe makes the ordering exact: the server's write blocks
// until the client reads, so a test decides when the server may make progress.
//
// The pin's `TestGossipFeedServesTheSampleAndStreams`, which drives the feed
// client through a real extender, is not ported: it needs the v2026 extender
// server seams (`GossipConnHandler`, `FeedConnHandler`, `IdentityKeySeed`) and
// the carrier dial behind `connect.DialExtenderFeed`, and neither the carrier
// layer nor the server port has landed in this fork (the carrier is a stub in
// net_extender_carrier.go).

package gossip

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Over the subscriber cap a client still gets its sample, and only the
// subscription is refused (D4).
func TestGossipFeedRefusesOverTheSubscriberCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	settings := DefaultFeedServerSettings()
	settings.MaxSubscriberCount = 1
	settings.KeepaliveTimeout = time.Minute
	feed := NewFeedServer(ctx, directory, nil, settings)
	t.Cleanup(feed.Close)

	held := newTestFeedClient(t, feed, true)
	held.readEndOfSample(t)
	if subscriberCount := feed.SubscriberCount(); subscriberCount != 1 {
		t.Fatalf("subscribers = %d, expected 1", subscriberCount)
	}

	// the second client is over the cap: it is served its sample and then the
	// stream ends
	refused := newTestFeedClient(t, feed, true)
	refused.readEndOfSample(t)
	select {
	case <-refused.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a client over the subscriber cap kept its subscription")
	}
	if _, err := connect.ReadExtenderFeedFrame(refused.conn); err == nil {
		t.Fatal("a client over the subscriber cap was still served")
	}

	// the held subscription is untouched
	select {
	case <-held.done:
		t.Fatal("the held subscription ended")
	default:
	}
}

// A subscriber that stops reading is disconnected rather than waited on, once
// it falls a whole buffer behind (D4).
func TestGossipFeedDisconnectsASlowSubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rootKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	settings := DefaultFeedServerSettings()
	// nothing else may end the stream while the buffer fills
	settings.KeepaliveTimeout = time.Minute
	settings.WriteTimeout = 0
	feed := NewFeedServer(ctx, directory, nil, settings)
	t.Cleanup(feed.Close)

	clientConn, serverConn := net.Pipe()
	barrier := &testFeedWriteBarrierConn{Conn: serverConn, entered: make(chan struct{}), proceed: make(chan struct{})}
	client := &testFeedClient{conn: clientConn, done: make(chan struct{})}
	go func() {
		defer close(client.done)
		defer serverConn.Close()
		feed.Serve(barrier)
	}()
	t.Cleanup(func() { barrier.release(); clientConn.Close(); <-client.done })
	if err := connect.WriteExtenderFeedRequest(client.conn, &protocol.ExtenderFeedRequest{Subscribe: true}); err != nil {
		t.Fatal(err)
	}
	client.readEndOfSample(t)
	barrier.armed.Store(true)

	// the server takes one message and blocks writing it, the buffer takes the
	// next, and the one after that overflows and cuts the subscription off
	issueTime := time.Now()
	extenderKey := newTestKey(t)
	first := signTestRecord(t, rootKey, extenderKey, "198.51.100.1", 443, issueTime)
	if _, err := directory.ApplyRecord(first, connect.ExtenderSourceGossip); err != nil {
		t.Fatal(err)
	}
	<-barrier.entered
	for i := range connect.ExtenderDirectorySubscribeBufferCount + 1 {
		record := signTestRecord(
			t,
			rootKey,
			extenderKey,
			"198.51.100.1",
			443,
			issueTime.Add(time.Duration(i+1)*time.Millisecond),
		)
		if _, err := directory.ApplyRecord(record, connect.ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
	}
	// releasing the blocked write is what lets the server see the closed
	// subscription
	barrier.release()
	if _, err := connect.ReadExtenderFeedFrame(client.conn); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a subscriber that fell behind was not disconnected")
	}
}

// Pins the first live write after the sample so queue overflow cannot depend
// on the server's scheduling or on changing partition membership.
type testFeedWriteBarrierConn struct {
	net.Conn
	armed       atomic.Bool
	entered     chan struct{}
	proceed     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

// Blocks an armed write before touching the underlying pipe.
func (self *testFeedWriteBarrierConn) Write(message []byte) (int, error) {
	if self.armed.Load() {
		self.enterOnce.Do(func() { close(self.entered) })
		<-self.proceed
	}
	return self.Conn.Write(message)
}

// Cleanup also releases the barrier if an assertion ends the test early.
func (self *testFeedWriteBarrierConn) release() {
	self.releaseOnce.Do(func() { close(self.proceed) })
}

// One feed client over a pipe, with the server half served in its own
// goroutine exactly as the extender serves it (A8).
type testFeedClient struct {
	conn net.Conn
	done chan struct{}
}

func newTestFeedClient(t *testing.T, feed *FeedServer, subscribe bool) *testFeedClient {
	t.Helper()
	return newTestFeedRequestClient(t, feed, &protocol.ExtenderFeedRequest{
		SampleCount: 8,
		Subscribe:   subscribe,
	})
}

// The same with the request the caller wants, for the cases that turn on what
// the client asked for.
func newTestFeedRequestClient(
	t *testing.T,
	feed *FeedServer,
	request *protocol.ExtenderFeedRequest,
) *testFeedClient {
	t.Helper()
	client := newTestFeedConn(t, feed)
	if err := connect.WriteExtenderFeedRequest(client.conn, request); err != nil {
		t.Fatal(err)
	}
	return client
}

// One served stream with nothing sent on it yet, for the cases that turn on
// the client saying nothing at all.
func newTestFeedConn(t *testing.T, feed *FeedServer) *testFeedClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	client := &testFeedClient{
		conn: clientConn,
		done: make(chan struct{}),
	}
	go func() {
		defer close(client.done)
		// the extender closes the stream when the handler returns (A8)
		defer serverConn.Close()
		feed.Serve(serverConn)
	}()
	t.Cleanup(func() {
		clientConn.Close()
		<-client.done
	})
	return client
}

// Reads through the sample, which leaves the stream on the live subscription.
func (self *testFeedClient) readEndOfSample(t *testing.T) {
	t.Helper()
	self.readSample(t)
}

// The frames of the sample, which are everything before `end_of_sample`.
func (self *testFeedClient) readSample(t *testing.T) []*protocol.ExtenderFeedFrame {
	t.Helper()
	frames := []*protocol.ExtenderFeedFrame{}
	for {
		frame, err := connect.ReadExtenderFeedFrame(self.conn)
		if err != nil {
			t.Fatal(err)
		}
		if frame.GetEndOfSample() {
			return frames
		}
		frames = append(frames, frame)
	}
}
