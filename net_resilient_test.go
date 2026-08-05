//go:build unix

package connect

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

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

// socketTtl reads the socket TTL through a dup'd fd, like the resilient path.
func socketTtl(t *testing.T, conn *net.TCPConn) int {
	t.Helper()
	f, err := conn.File()
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	defer f.Close()
	return GetSocketTtl(SocketHandle(f.Fd()))
}

// setSocketTtl sets the socket TTL to ttl for the duration of the test.
// The restore is best-effort: it runs during cleanup, after which the conn
// may already be closed, so a failure there must not fail the test.
func setSocketTtl(t *testing.T, conn *net.TCPConn, ttl int) {
	t.Helper()
	f, err := conn.File()
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	SetSocketTtl(SocketHandle(f.Fd()), ttl)
	f.Close()
	t.Cleanup(func() {
		f, err := conn.File()
		if err != nil {
			return // conn already closed; nothing to restore
		}
		SetSocketTtl(SocketHandle(f.Fd()), 64)
		f.Close()
	})
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

func TestResilientTlsConnFragmentRestoresTtlAndClosesFd(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, server := newTcpPair(t)
	setSocketTtl(t, client, 42)

	rconn := NewResilientTlsConn(client, true, true)
	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}

	// The socket TTL must be restored to the native value after the
	// fragment write, on the success path.
	if got := socketTtl(t, client); got != 42 {
		t.Fatalf("socket TTL after fragmented write = %d, want 42 (native restored)", got)
	}

	// The peer must receive the full record payload (fragmentation re-frames
	// the payload into standalone TLS records, so payloads concatenate back
	// to the original handshake bytes).
	got := readTlsRecords(t, server, len(record)-5)
	if !bytes.Equal(got, record[5:]) {
		t.Fatalf("peer received different payload than written")
	}
}

func TestResilientTlsConnFragmentFailureDisablesAndRestoresTtl(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, server := newTcpPair(t)
	setSocketTtl(t, client, 42)

	// Expire the write deadline so the first fragment write fails
	// deterministically after the fd and native TTL are acquired.
	client.SetWriteDeadline(time.Now().Add(-time.Second))

	rconn := NewResilientTlsConn(client, true, true)
	_, err := rconn.Write(record)
	if err == nil {
		t.Fatalf("write with expired deadline: expected error, got nil")
	}

	// The layer must be disabled so a retry cannot re-fragment the
	// partially-sent record, and the buffered record must be dropped.
	if rconn.enabled {
		t.Fatalf("layer still enabled after fragment write failure")
	}
	if len(rconn.buffer) != 0 {
		t.Fatalf("buffer not dropped after fragment write failure: %d bytes", len(rconn.buffer))
	}

	// The socket TTL must be restored even on the failure path.
	client.SetWriteDeadline(time.Time{})
	if got := socketTtl(t, client); got != 42 {
		t.Fatalf("socket TTL after failed fragmented write = %d, want 42 (restored)", got)
	}

	// A subsequent Write goes through the disabled path directly, so the
	// peer receives the raw record byte-for-byte.
	client.SetWriteDeadline(time.Time{})
	if _, err := rconn.Write(record); err != nil {
		t.Fatalf("write after disable: %v", err)
	}
	got := make([]byte, len(record))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("read after disable: %v", err)
	}
	if !bytes.Equal(got, record) {
		t.Fatalf("peer received different bytes after disable")
	}
}

func TestResilientTlsConnOffDrainsPartialRecord(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, server := newTcpPair(t)

	rconn := NewResilientTlsConn(client, true, false)

	// Write only part of a record: the header and 10 payload bytes. Write
	// returns len(b), nil and buffers the rest.
	partial := record[:15]
	n, err := rconn.Write(partial)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(partial) {
		t.Fatalf("write n=%d want %d", n, len(partial))
	}
	if len(rconn.buffer) != len(partial) {
		t.Fatalf("buffered %d bytes, want %d", len(rconn.buffer), len(partial))
	}

	// Off must drain the buffered bytes to the wire before disabling, so
	// the bytes an earlier Write accepted are not stranded.
	rconn.Off()
	if rconn.enabled {
		t.Fatalf("layer still enabled after Off")
	}
	if len(rconn.buffer) != 0 {
		t.Fatalf("buffer not drained by Off: %d bytes", len(rconn.buffer))
	}

	got := make([]byte, len(partial))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, partial) {
		t.Fatalf("peer received different bytes than the drained partial record")
	}
}

// countOpenFds counts the process's open file descriptors via /proc.
func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

func TestResilientTlsConnFragmentDoesNotLeakFileDescriptors(t *testing.T) {
	record := buildClientHelloRecord(t)

	before := countOpenFds(t)
	for i := 0; i < 20; i++ {
		client, server := newTcpPair(t)
		client.SetWriteDeadline(time.Now().Add(-time.Second))
		rconn := NewResilientTlsConn(client, true, true)
		if _, err := rconn.Write(record); err == nil {
			t.Fatalf("iteration %d: expected write error, got nil", i)
		}
		client.Close()
		server.Close()
	}
	after := countOpenFds(t)
	if after > before+5 {
		t.Fatalf("file descriptors grew from %d to %d over 20 failed fragment writes", before, after)
	}
}

func TestResilientTlsConnReorderOnlyFragmentsOnFailure(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, _ := newTcpPair(t)
	setSocketTtl(t, client, 42)

	client.SetWriteDeadline(time.Now().Add(-time.Second))
	rconn := NewResilientTlsConn(client, false, true)
	_, err := rconn.Write(record)
	if err == nil {
		t.Fatalf("write with expired deadline: expected error, got nil")
	}
	if rconn.enabled {
		t.Fatalf("layer still enabled after reorder write failure")
	}
	client.SetWriteDeadline(time.Time{})
	if got := socketTtl(t, client); got != 42 {
		t.Fatalf("socket TTL after failed reorder write = %d, want 42 (restored)", got)
	}
}
