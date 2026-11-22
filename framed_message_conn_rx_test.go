package connect

import (
	"io"
	"net"
	"testing"
	"time"
)

// The framed carrier counts what it reads as well as what it writes, so a
// reader can tell the two directions apart.
func TestFramedMessageConnCountsReceiveBytes(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	stats := &H1PlusStats{}
	reader, err := NewFramedMessageConn(clientSide, H1FramerProtocol, 1024, stats)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewFramedMessageConn(serverSide, H1FramerProtocol, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}

	messages := [][]byte{[]byte("hello"), []byte("world!")}
	writeErr := make(chan error, 1)
	go func() { writeErr <- writer.WriteMessages(messages) }()

	for i, want := range messages {
		kind, got, err := reader.ReadPooledMessage()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if kind != 2 { // websocket.BinaryMessage
			t.Fatalf("read %d kind = %d", i, kind)
		}
		if string(got) != string(want) {
			t.Fatalf("read %d = %q, want %q", i, got, want)
		}
		MessagePoolReturn(got)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}

	snapshot := stats.Snapshot()
	if snapshot.RxMessages != 2 {
		t.Fatalf("rx messages = %d, want 2", snapshot.RxMessages)
	}
	if snapshot.RxBytes != 11 {
		t.Fatalf("rx bytes = %d, want 11", snapshot.RxBytes)
	}
	// The write side of this collector saw nothing; its counters stay put.
	if snapshot.Messages != 0 || snapshot.Bytes != 0 {
		t.Fatalf("write counters moved: messages=%d bytes=%d", snapshot.Messages, snapshot.Bytes)
	}
}

// The unpooled read path funnels through the same counter.
func TestFramedMessageConnCountsUnpooledReads(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	stats := &H1PlusStats{}
	reader, err := NewFramedMessageConn(clientSide, H1FramerProtocol, 1024, stats)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewFramedMessageConn(serverSide, H1FramerProtocol, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { writer.WriteMessages([][]byte{[]byte("ping")}) }()
	if _, message, err := reader.ReadMessage(); err != nil || string(message) != "ping" {
		t.Fatalf("read = %q, %v", message, err)
	}
	snapshot := stats.Snapshot()
	if snapshot.RxMessages != 1 || snapshot.RxBytes != 4 {
		t.Fatalf("rx = %d messages / %d bytes, want 1 / 4", snapshot.RxMessages, snapshot.RxBytes)
	}
}

// replayConn serves a fixed byte stream and can be rewound, so a benchmark
// measures the framer read path rather than a pipe round trip.
type replayConn struct {
	data []byte
	off  int
}

// Read repeats the stream forever, so a benchmark can read as many messages as
// it likes from a stream built once. The stream is a whole number of frames, so
// the repeat is still a valid framed stream.
func (c *replayConn) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		written := copy(p[n:], c.data[c.off:])
		n += written
		c.off = (c.off + written) % len(c.data)
	}
	return n, nil
}

// Write collects the framed stream a builder connection writes.
func (c *replayConn) Write(p []byte) (int, error) {
	c.data = append(c.data, p...)
	return len(p), nil
}
func (c *replayConn) Close() error                     { return nil }
func (c *replayConn) LocalAddr() net.Addr              { return nil }
func (c *replayConn) RemoteAddr() net.Addr             { return nil }
func (c *replayConn) SetDeadline(time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(time.Time) error { return nil }

func buildFramedStream(b *testing.B, messages int, payload []byte) []byte {
	b.Helper()
	sink := &replayConn{}
	writer, err := NewFramedMessageConn(sink, H1FramerProtocol, 1024, nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < messages; i++ {
		if err := writer.WriteMessages([][]byte{payload}); err != nil {
			b.Fatal(err)
		}
	}
	return sink.data
}

func benchmarkFramedRead(b *testing.B, stats *H1PlusStats) {
	const messages = 256
	payload := make([]byte, 256)
	stream := buildFramedStream(b, messages, payload)
	b.SetBytes(int64(messages * len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	conn := &replayConn{data: stream}
	reader, err := NewFramedMessageConn(conn, H1FramerProtocol, 1024, stats)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		for j := 0; j < messages; j++ {
			_, message, err := reader.ReadPooledMessage()
			if err != nil {
				b.Fatal(err)
			}
			MessagePoolReturn(message)
		}
	}
}

func BenchmarkFramedReadCounted(b *testing.B)   { benchmarkFramedRead(b, &H1PlusStats{}) }
func BenchmarkFramedReadUncounted(b *testing.B) { benchmarkFramedRead(b, nil) }
