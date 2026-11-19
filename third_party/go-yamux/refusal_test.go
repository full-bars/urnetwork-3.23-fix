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
	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
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
	laterAccepted, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("accept after memory was free again: %v", err)
	}
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
