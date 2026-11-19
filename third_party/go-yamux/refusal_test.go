package yamux

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

var errTestMemoryRefused = errors.New("test memory refused")

// A memory manager whose spans reserve nothing while `refuse` is set.
type refusingMemoryManager struct {
	refuse *atomic.Bool
}

func (m refusingMemoryManager) ReserveMemory(size int, prio uint8) error {
	if m.refuse.Load() {
		return errTestMemoryRefused
	}
	return nil
}

func (m refusingMemoryManager) ReleaseMemory(size int) {}

func (m refusingMemoryManager) Done() {}

func testSessionPair(t *testing.T, newServerMemoryManager func() (MemoryManager, error)) (client *Session, server *Session) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverConn, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}

	config := DefaultConfig()
	config.LogOutput = io.Discard
	client, err = Client(clientConn, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err = Server(serverConn, config, newServerMemoryManager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

// acceptStreamBounded accepts a stream and fails the test, instead of waiting
// for the package timeout, when none arrives. It closes both sessions to
// release the pending accept.
func acceptStreamBounded(t *testing.T, client *Session, server *Session, phase string) *Stream {
	t.Helper()
	type result struct {
		stream *Stream
		err    error
	}
	results := make(chan result, 1)
	go func() {
		stream, err := server.AcceptStream()
		results <- result{stream, err}
	}()
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("%s: accept: %v", phase, r.err)
		}
		return r.stream
	case <-time.After(10 * time.Second):
		client.Close()
		server.Close()
		t.Fatalf("%s: no stream accepted within 10 seconds", phase)
		return nil
	}
}

// A refused stream is reset on its own. The session and the streams already
// open on it carry on, and a later stream is accepted once memory is free.
func testRefusedStreamLeavesSession(t *testing.T, newServerMemoryManager func() (MemoryManager, error), refuse *atomic.Bool) {
	client, server := testSessionPair(t, newServerMemoryManager)

	healthy, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := healthy.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	accepted := acceptStreamBounded(t, client, server, "first stream")
	buf := make([]byte, 1)
	if _, err := io.ReadFull(accepted, buf); err != nil {
		t.Fatal(err)
	}

	refuse.Store(true)
	refused, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refused.Write([]byte("b"))
	refused.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := refused.Read(buf); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("refused stream read = %v, want ErrStreamReset", err)
	}

	if client.IsClosed() || server.IsClosed() {
		t.Fatalf("session closed after refusing one stream (client %v, server %v)", client.IsClosed(), server.IsClosed())
	}
	if _, err := accepted.Write([]byte("c")); err != nil {
		t.Fatalf("write on the stream open before the refusal: %v", err)
	}
	healthy.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(healthy, buf); err != nil || buf[0] != 'c' {
		t.Fatalf("read on the stream open before the refusal = %q, %v", buf, err)
	}

	refuse.Store(false)
	later, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := later.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	laterAccepted := acceptStreamBounded(t, client, server, "stream after memory was free again")
	if _, err := io.ReadFull(laterAccepted, buf); err != nil || buf[0] != 'd' {
		t.Fatalf("read on the later stream = %q, %v", buf, err)
	}
}

func TestIncomingStreamReserveRefusedResetsOnlyThatStream(t *testing.T) {
	refuse := &atomic.Bool{}
	testRefusedStreamLeavesSession(t, func() (MemoryManager, error) {
		return refusingMemoryManager{refuse: refuse}, nil
	}, refuse)
}

func TestIncomingStreamSpanRefusedResetsOnlyThatStream(t *testing.T) {
	refuse := &atomic.Bool{}
	testRefusedStreamLeavesSession(t, func() (MemoryManager, error) {
		if refuse.Load() {
			return nil, errTestMemoryRefused
		}
		return refusingMemoryManager{refuse: refuse}, nil
	}, refuse)
}

// A SYN that reuses the ID of a registered stream is a protocol error. When
// memory is also refused, the refusal must not reset and dispatch it to the
// existing stream: the cause is returned, which ends the session as it did
// before the patch. A refused SYN with a new ID is only reset.
func TestIncomingStreamRefusedDuplicateIDReturnsTheCause(t *testing.T) {
	refuse := &atomic.Bool{}
	client, server := testSessionPair(t, func() (MemoryManager, error) {
		return refusingMemoryManager{refuse: refuse}, nil
	})

	healthy, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := healthy.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	accepted := acceptStreamBounded(t, client, server, "first stream")

	refuse.Store(true)
	if err := server.incomingStream(accepted.id); !errors.Is(err, errTestMemoryRefused) {
		t.Fatalf("refused duplicate id returned %v, want the refusal cause", err)
	}
	if err := server.incomingStream(accepted.id + 2); err != nil {
		t.Fatalf("refused new id returned %v, want nil", err)
	}
}
