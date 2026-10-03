package connect

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// newTcpPair returns a connected TCP client/server pair on loopback.
func newTcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverCh := make(chan *net.TCPConn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		serverCh <- conn.(*net.TCPConn)
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var server *net.TCPConn
	select {
	case server = <-serverCh:
	case err := <-errCh:
		t.Fatalf("accept: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("accept timeout")
	}
	// the listener is only needed to accept; close it so tests that create
	// many pairs (e.g. the fd-leak check) do not accumulate listener fds
	ln.Close()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client.(*net.TCPConn), server
}

// buildClientHelloRecord builds a TLS record (content type 22) carrying a
// ClientHello with a server_name extension, the shape UnmarshalClientHello
// needs to route into the fragment/reorder path.
func buildClientHelloRecord(t *testing.T) []byte {
	t.Helper()

	var clientHello bytes.Buffer
	clientHello.Write([]byte{0x03, 0x03}) // version TLS 1.2
	clientHello.Write(make([]byte, 32))   // random
	clientHello.WriteByte(0)              // session id length
	clientHello.Write([]byte{0x00, 0x02}) // cipher suites length
	clientHello.Write([]byte{0x13, 0x01}) // TLS_AES_128_GCM_SHA256
	clientHello.WriteByte(1)              // compression methods length
	clientHello.WriteByte(0)              // null

	var serverName bytes.Buffer
	serverName.WriteByte(0)              // host_name
	serverName.Write([]byte{0x00, 0x0b}) // name length
	serverName.WriteString("example.com")
	var sniList bytes.Buffer
	binary.Write(&sniList, binary.BigEndian, uint16(serverName.Len()))
	sniList.Write(serverName.Bytes())

	var extensions bytes.Buffer
	binary.Write(&extensions, binary.BigEndian, uint16(0)) // server_name extension type
	binary.Write(&extensions, binary.BigEndian, uint16(sniList.Len()))
	extensions.Write(sniList.Bytes())

	binary.Write(&clientHello, binary.BigEndian, uint16(extensions.Len()))
	clientHello.Write(extensions.Bytes())

	var handshake bytes.Buffer
	handshake.WriteByte(1) // ClientHello
	// uint24 length, written byte-wise (PutUint32 needs 4 bytes)
	l := clientHello.Len()
	handshake.WriteByte(byte(l >> 16))
	handshake.WriteByte(byte(l >> 8))
	handshake.WriteByte(byte(l))
	handshake.Write(clientHello.Bytes())

	record := make([]byte, 0, 5+handshake.Len())
	record = append(record, 22) // handshake content type
	record = append(record, 0x03, 0x03)
	binary.BigEndian.PutUint16(append([]byte{}, 0, 0), uint16(handshake.Len()))
	record = append(record, byte(handshake.Len()>>8), byte(handshake.Len()))
	record = append(record, handshake.Bytes()...)

	if _, meta := UnmarshalClientHello(handshake.Bytes()); meta == nil || meta.ServerNameValueEnd <= meta.ServerNameValueStart {
		t.Fatalf("test ClientHello did not parse into the fragment path")
	}
	return record
}

// socketTtl reads the socket TTL through SyscallConn, like the resilient path.
//
// These helpers deliberately avoid TCPConn.File plus os.File.Fd. Fd puts the
// socket into blocking mode, which detaches it from the runtime poller and
// stops write deadlines from being enforced. Several tests below fail a write
// by expiring the write deadline, so a helper that quietly switched the socket
// to blocking mode would defeat the failure injection and let those tests pass
// without exercising the failure path at all.
func socketTtl(t *testing.T, conn *net.TCPConn) int {
	t.Helper()
	ttlCtl, err := newTtlControl(conn)
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	return ttlCtl.get()
}

// setSocketTtl sets the socket TTL to ttl for the duration of the test.
// The restore is best-effort: it runs during cleanup, after which the conn
// may already be closed, so a failure there must not fail the test.
func setSocketTtl(t *testing.T, conn *net.TCPConn, ttl int) {
	t.Helper()
	ttlCtl, err := newTtlControl(conn)
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	if err := ttlCtl.set(ttl, nil); err != nil {
		t.Fatalf("set ttl %d: %v", ttl, err)
	}
	t.Cleanup(func() { _ = ttlCtl.set(64, nil) })
}

// readTlsRecords reads raw TLS records from r and returns their
// concatenated payloads. The resilient fragment path re-frames a record's
// payload as multiple standalone TLS records, so reassembly is required to
// compare against the original payload.
func readTlsRecords(t *testing.T, r io.Reader, wantPayloadLen int) []byte {
	t.Helper()
	var payload []byte
	for len(payload) < wantPayloadLen {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			t.Fatalf("read record header: %v", err)
		}
		if hdr[0] != 22 {
			t.Fatalf("record content type = %d, want 22", hdr[0])
		}
		recLen := int(hdr[3])<<8 | int(hdr[4])
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(r, rec); err != nil {
			t.Fatalf("read record body: %v", err)
		}
		payload = append(payload, rec...)
	}
	return payload
}
