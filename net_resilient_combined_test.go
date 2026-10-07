//go:build unix

package connect

// net_resilient_combined_test.go — the combined TCP-segment + TLS-record
// fragmentation mode (net_resilient.go `segment`). The root-cause test is a
// reassembling-middlebox model that blocks a hello fragmented by a single
// method (record OR segment) but is slipped by the combined pair, matching the
// foci 2025 finding that only the combination beats russia's tspu reassembly.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"slices"
	"syscall"
	"testing"
	"time"
)

// segmentRecordingConn records each Write as one tcp segment and is
// deliberately NOT a *net.TCPConn, so ResilientTlsConn.Write takes the
// userspace fragment path (no raw sockets, no ttl) -- the ios network
// extension and non-root android case the combined mode must work in. It is
// driven from a single goroutine, so it needs no locking.
//
// failOnWrite, when positive, makes that (1-based) Write return failErr after
// accepting none of its bytes, so a test can accept the first half of a cut
// record and refuse the second.
type segmentRecordingConn struct {
	segments    [][]byte
	failOnWrite int
	failErr     error
	closed      bool
}

func (self *segmentRecordingConn) Write(b []byte) (int, error) {
	self.segments = append(self.segments, slices.Clone(b))
	if 0 < self.failOnWrite && len(self.segments) == self.failOnWrite {
		return 0, self.failErr
	}
	return len(b), nil
}

func (self *segmentRecordingConn) Read(b []byte) (int, error)       { return 0, nil }
func (self *segmentRecordingConn) Close() error                     { self.closed = true; return nil }
func (self *segmentRecordingConn) LocalAddr() net.Addr              { return nil }
func (self *segmentRecordingConn) RemoteAddr() net.Addr             { return nil }
func (self *segmentRecordingConn) SetDeadline(time.Time) error      { return nil }
func (self *segmentRecordingConn) SetReadDeadline(time.Time) error  { return nil }
func (self *segmentRecordingConn) SetWriteDeadline(time.Time) error { return nil }

// writeThroughResilient writes record through a resilient conn in the given
// mode and returns the tcp segments it emitted (one per Write).
func writeThroughResilient(t *testing.T, record []byte, fragment, reorder, segment bool) [][]byte {
	t.Helper()
	conn := &segmentRecordingConn{}
	rconn := newResilientTlsConn(conn, fragment, reorder, segment)
	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}
	return conn.segments
}

// concatSegments joins segment payloads back into the on-wire byte stream.
func concatSegments(segments [][]byte) []byte {
	var stream []byte
	for _, segment := range segments {
		stream = append(stream, segment...)
	}
	return stream
}

// parseTlsRecordStream walks stream as a sequence of whole tls records,
// returning the record count, the concatenated handshake-record payloads, and
// the byte offset at the end of each record. A record that runs past the
// stream, or trailing bytes, fail the test: the combined mode must emit whole,
// well-framed records (only their placement across tcp segments changes).
func parseTlsRecordStream(t *testing.T, stream []byte) (recordCount int, handshakePayload []byte, recordEndOffsets []int) {
	t.Helper()
	offset := 0
	for offset+5 <= len(stream) {
		header := parseTlsHeader(stream[offset : offset+5])
		end := offset + 5 + int(header.contentLength)
		if len(stream) < end {
			t.Fatalf("record at %d runs past the stream (have %d, want %d)", offset, len(stream), end)
		}
		if header.contentType == byte(TlsContentTypeHandshake) {
			handshakePayload = append(handshakePayload, stream[offset+5:end]...)
		}
		recordCount += 1
		offset = end
		recordEndOffsets = append(recordEndOffsets, offset)
	}
	if offset != len(stream) {
		t.Fatalf("trailing %d bytes after the last record", len(stream)-offset)
	}
	return
}

// segmentEndOffsets is the byte offset at the end of each tcp segment.
func segmentEndOffsets(segments [][]byte) []int {
	offsets := []int{}
	sum := 0
	for _, segment := range segments {
		sum += len(segment)
		offsets = append(offsets, sum)
	}
	return offsets
}

// chopIntoSegments splits data into fixed-size tcp segments WITHOUT re-framing
// it into more tls records: a single record spread across segments, which is
// tcp segmentation alone.
func chopIntoSegments(data []byte, size int) [][]byte {
	segments := [][]byte{}
	for offset := 0; offset < len(data); offset += size {
		end := min(offset+size, len(data))
		segments = append(segments, slices.Clone(data[offset:end]))
	}
	return segments
}

// streamReassemblyExposesSni is detector A of the reassembling-middlebox
// model (foci 2025, foci-2025-0016): reassemble the whole tcp stream and scan
// for the sni as a contiguous substring. It sees through tcp segmentation.
func streamReassemblyExposesSni(segments [][]byte, serverName string) bool {
	return bytes.Contains(concatSegments(segments), []byte(serverName))
}

// segmentLocalRecordsExposeSni is detector B: within each segment
// independently, read the whole tls records it fully contains, collect their
// handshake payloads and scan the concatenation for the sni. It sees through
// record fragmentation that aligns a record to its segment, and stitches
// nothing across a segment boundary. A hello whose every record is cut across
// two segments hides from it. This is a model, not the real tspu.
func segmentLocalRecordsExposeSni(segments [][]byte, serverName string) bool {
	var handshakePayload []byte
	for _, segment := range segments {
		rest := segment
		for 5 <= len(rest) {
			header := parseTlsHeader(rest[0:5])
			end := 5 + int(header.contentLength)
			if len(rest) < end {
				// the record is not whole within this segment
				break
			}
			if header.contentType == byte(TlsContentTypeHandshake) {
				handshakePayload = append(handshakePayload, rest[5:end]...)
			}
			rest = rest[end:]
		}
	}
	return bytes.Contains(handshakePayload, []byte(serverName))
}

// TestWriteRecordMaybeSegmentedGeometry pins the primitive: with segment on, a
// record of two or more bytes becomes exactly two writes cut at len/2; a
// shorter record is a single write; with segment off it is always one write.
func TestWriteRecordMaybeSegmentedGeometry(t *testing.T) {
	cases := []struct {
		name      string
		segment   bool
		recordLen int
		want      []int
	}{
		{"even", true, 8, []int{4, 4}},
		{"odd", true, 13, []int{6, 7}},
		{"min", true, combinedSegmentMinLen, []int{1, 1}},
		{"belowMin", true, combinedSegmentMinLen - 1, []int{1}},
		{"segmentOff", false, 13, []int{13}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := make([]byte, tc.recordLen)
			for i := range record {
				record[i] = byte(i + 1)
			}
			conn := &segmentRecordingConn{}
			rconn := newResilientTlsConn(conn, true, false, tc.segment)
			if err := rconn.writeRecordMaybeSegmented(conn, record); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := []int{}
			for _, segment := range conn.segments {
				got = append(got, len(segment))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("write sizes = %v, want %v", got, tc.want)
			}
			if !bytes.Equal(concatSegments(conn.segments), record) {
				t.Fatal("the writes do not reassemble the record")
			}
		})
	}
}

// TestResilientCombinedEmitsRecordsAndSegments asserts the combined dial emits
// BOTH multiple tls records AND multiple tcp segments for one hello, that no
// whole record sits in one segment, that segment boundaries are distinct from
// the record boundaries (a segment is cut inside a record), and that the
// bytes are preserved.
func TestResilientCombinedEmitsRecordsAndSegments(t *testing.T) {
	record := buildClientHelloRecord(t)
	segments := writeThroughResilient(t, record, true, false, true)

	stream := concatSegments(segments)
	recordCount, handshakePayload, recordEndOffsets := parseTlsRecordStream(t, stream)
	if recordCount < 2 {
		t.Fatalf("combined emitted %d tls records, want several", recordCount)
	}
	if len(segments) != 2*recordCount {
		t.Fatalf("combined emitted %d tcp segments for %d records, want exactly two per record", len(segments), recordCount)
	}
	if !bytes.Equal(handshakePayload, record[5:]) {
		t.Fatal("the combined fragments do not reassemble into the hello handshake")
	}

	recordEndSet := map[int]bool{}
	for _, offset := range recordEndOffsets {
		recordEndSet[offset] = true
	}
	segmentOffsets := segmentEndOffsets(segments)
	interior := 0
	for _, offset := range segmentOffsets[:len(segmentOffsets)-1] {
		if !recordEndSet[offset] {
			interior += 1
		}
	}
	if interior != recordCount {
		t.Fatalf("%d segment boundaries fell inside a record, want one per record (%d); segments %v, record ends %v", interior, recordCount, segmentOffsets, recordEndOffsets)
	}

	// no segment holds a whole record: each starts a record or ends one, never both
	start := 0
	for i, offset := range segmentOffsets {
		startsRecord := start == 0 || slices.Contains(recordEndOffsets, start)
		endsRecord := recordEndSet[offset]
		if startsRecord && endsRecord {
			t.Fatalf("segment %d [%d,%d) contains a whole tls record", i, start, offset)
		}
		start = offset
	}
}

// TestCombinedModeDefeatsReassemblingMiddlebox is the root-cause test. Record
// fragmentation alone is recovered by the segment-local record reader (B), tcp
// segmentation alone by stream reassembly (A); the combined mode leaves the
// segment-local reader nothing to read, and the sni never sits whole in one
// segment. (Detector A is not asserted on the combined output: the hello's
// split points are random, so the sni may by chance stay inside one record,
// which a stream reassembler would find. That is the stream-reassembler half
// of the model, which the combined mode only defeats together with the
// random record split.)
func TestCombinedModeDefeatsReassemblingMiddlebox(t *testing.T) {
	record := buildClientHelloRecord(t)
	const serverName = "example.com" // buildClientHelloRecord's sni

	fragmentOnly := writeThroughResilient(t, record, true, false, false)
	if !segmentLocalRecordsExposeSni(fragmentOnly, serverName) {
		t.Fatal("record fragmentation alone slipped the segment-local reader; the model cannot discriminate the combined mode")
	}

	tcpSegmentationOnly := chopIntoSegments(record, 40)
	if len(tcpSegmentationOnly) < 2 {
		t.Fatalf("the tcp-segmentation model produced %d segments, want several", len(tcpSegmentationOnly))
	}
	if !streamReassemblyExposesSni(tcpSegmentationOnly, serverName) {
		t.Fatal("tcp segmentation alone slipped stream reassembly; the model cannot discriminate the combined mode")
	}

	combined := writeThroughResilient(t, record, true, false, true)
	if segmentLocalRecordsExposeSni(combined, serverName) {
		t.Fatal("the combined mode was read by the segment-local record reader; no record may sit whole in a segment")
	}
	for i, segment := range combined {
		if bytes.Contains(segment, []byte(serverName)) {
			t.Fatalf("segment %d carries the whole sni", i)
		}
	}
}

// TestResilientCombinedComposesWithTtlReorder checks that turning the combined
// tcp segmentation on does not disturb the reorder ttl choreography on a real
// socket: the fragment+reorder+segment path still lowers the first fragment's
// ttl, restores the native ttl, and the peer still receives the whole hello.
func TestResilientCombinedComposesWithTtlReorder(t *testing.T) {
	record := buildClientHelloRecord(t)
	client, server := newTcpPair(t)
	nativeTtl := 42
	setSocketTtl(t, client, nativeTtl)

	seam := &ttlSeam{passthrough: true}
	rconn := newResilientTlsConn(client, true, true, true)
	rconn.setTtl = seam.set

	n, err := rconn.Write(record)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(record) {
		t.Fatalf("write n=%d want %d", n, len(record))
	}
	if rconn.ttlErr != nil {
		t.Fatalf("ttlErr = %v, want nil", rconn.ttlErr)
	}
	if len(seam.applied) < 2 || seam.applied[0] != resilientLowTtl {
		t.Fatalf("applied ttl sequence = %v, want it to begin with resilientLowTtl=%d", seam.applied, resilientLowTtl)
	}
	if last := seam.applied[len(seam.applied)-1]; last != nativeTtl {
		t.Fatalf("applied ttl sequence = %v, want it to end with the native ttl %d", seam.applied, nativeTtl)
	}
	if got := socketTtl(t, client); got != nativeTtl {
		t.Fatalf("socket ttl after combined+reorder write = %d, want %d", got, nativeTtl)
	}

	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := readTlsRecords(t, server, len(record)-5)
	if !bytes.Equal(got, record[5:]) {
		t.Fatal("peer received different payload than the hello")
	}
}

// TestResilientCombinedFailsClosedOnSecondHalf is the fork-specific fail-closed
// check: the connection accepts the first half of a cut record and errors the
// second. The error must propagate, the second half must never be retried, and
// the connection must be failed (layer disabled, buffer dropped, conn closed).
func TestResilientCombinedFailsClosedOnSecondHalf(t *testing.T) {
	record := buildClientHelloRecord(t)
	boom := errors.New("second half refused")
	conn := &segmentRecordingConn{failOnWrite: 2, failErr: boom}
	rconn := newResilientTlsConn(conn, true, false, true)

	_, err := rconn.Write(record)
	if !errors.Is(err, boom) {
		t.Fatalf("write error = %v, want %v", err, boom)
	}
	if len(conn.segments) != 2 {
		t.Fatalf("conn saw %d writes, want 2: the failed second half must not be retried and nothing may follow it", len(conn.segments))
	}
	if rconn.enabled {
		t.Fatal("layer still enabled after a failed second half")
	}
	if len(rconn.buffer) != 0 {
		t.Fatalf("buffer not dropped after a failed second half: %d bytes", len(rconn.buffer))
	}
	if !conn.closed {
		t.Fatal("underlying connection not closed after a failed second half")
	}
}

// TestResilientCombinedFirstHalfFailureSkipsSecond covers the other fail-closed
// edge: a refused first half never reaches the second.
func TestResilientCombinedFirstHalfFailureSkipsSecond(t *testing.T) {
	record := []byte{byte(TlsContentTypeHandshake), 0x03, 0x03, 0x00, 0x08, 1, 2, 3, 4, 5, 6, 7, 8}
	conn := &segmentRecordingConn{failOnWrite: 1, failErr: io.ErrClosedPipe}
	rconn := newResilientTlsConn(conn, true, false, true)

	if err := rconn.writeRecordMaybeSegmented(conn, record); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error = %v, want %v", err, io.ErrClosedPipe)
	}
	if len(conn.segments) != 1 {
		t.Fatalf("conn saw %d writes, want 1: the second half must never be written after a first-half failure", len(conn.segments))
	}
}

func tcpNoDelay(t *testing.T, conn *net.TCPConn) bool {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	var value int
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		value, sockErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if sockErr != nil {
		t.Fatalf("getsockopt TCP_NODELAY: %v", sockErr)
	}
	return value != 0
}

// TestResilientCombinedPlainFragmentSetsNoDelay checks the plain-fragment TCP
// branch: with segment on, NODELAY is set so Nagle cannot coalesce the two
// halves; with segment off the socket option is left alone (so the test also
// proves the option change comes from segment, not from Go's default).
func TestResilientCombinedPlainFragmentSetsNoDelay(t *testing.T) {
	for _, segment := range []bool{true, false} {
		record := buildClientHelloRecord(t)
		client, server := newTcpPair(t)
		if err := client.SetNoDelay(false); err != nil {
			t.Fatalf("clear nodelay: %v", err)
		}

		rconn := newResilientTlsConn(client, true, false, segment)
		if _, err := rconn.Write(record); err != nil {
			t.Fatalf("segment=%v write: %v", segment, err)
		}
		if got := tcpNoDelay(t, client); got != segment {
			t.Fatalf("segment=%v: TCP_NODELAY = %v, want %v", segment, got, segment)
		}

		server.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := readTlsRecords(t, server, len(record)-5)
		if !bytes.Equal(got, record[5:]) {
			t.Fatalf("segment=%v: peer received different payload than the hello", segment)
		}
	}
}

// TestNewResilientDialTlsContextConstructorsBackCompat keeps the exported
// constructors compiling and working with their pre-segment signatures.
func TestNewResilientDialTlsContextConstructorsBackCompat(t *testing.T) {
	conn := &segmentRecordingConn{}
	if rconn := NewResilientTlsConn(conn, true, false); rconn.segment {
		t.Fatal("NewResilientTlsConn must leave segment off")
	}
	settings := DefaultConnectSettings()
	if NewResilientDialTlsContext(settings, true, false) == nil {
		t.Fatal("NewResilientDialTlsContext returned nil")
	}
	if NewResilientDialTlsContextWithSegment(settings, true, false, true) == nil {
		t.Fatal("NewResilientDialTlsContextWithSegment returned nil")
	}
}
