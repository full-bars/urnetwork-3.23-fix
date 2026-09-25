package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A direct (unproxied) node counted billable bytes but never total bytes:
// TotalRx/TotalTx were credited only by trackedConn, and trackedConn was built
// only by the SOCKS5 and HTTP proxy dialers. On a direct-only node the total
// graph, the total rate and the session "moved" figure were therefore always
// zero while billable was not. These tests pin that a direct socket is counted.

func waitBytes(t *testing.T, what string, get func() uint64, want uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", what, get(), want)
}

func TestUdpSequenceDirectSocketCountsTotalBytes(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dest := listener.LocalAddr().(*net.UDPAddr)

	const request, reply = "direct-udp-request", "direct-udp-echo-reply-longer"
	go func() {
		buf := make([]byte, 2048)
		n, from, err := listener.ReadFromUDP(buf)
		if err == nil && n > 0 {
			listener.WriteToUDP([]byte(reply), from)
		}
	}()

	settings := DefaultUdpBufferSettings()
	settings.IdleTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotReply := make(chan struct{}, 1)
	seq := NewUdpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
			select {
			case gotReply <- struct{}{}:
			default:
			}
		},
		DestinationId(NewId()),
		protocol.ProvideMode_Network,
		4,
		net.ParseIP("127.0.0.1"), UDPPort(40001),
		dest.IP, UDPPort(dest.Port),
		settings,
	)
	bw := &ProxyBandwidth{}
	seq.bw = bw

	done := make(chan struct{})
	go func() { seq.Run(); close(done) }()

	udp := &parsedUdp{}
	udp.payload = []byte(request)
	item := &UdpSendItem{udp: udp, ipPacket: MessagePoolGet(2048)}
	if ok, _ := seq.send(item, time.Second); !ok {
		t.Fatal("send was not accepted")
	}
	select {
	case <-gotReply:
	case <-time.After(3 * time.Second):
		t.Fatal("no reply came back through the sequence")
	}

	waitBytes(t, "TotalTx", bw.TotalTx.Load, uint64(len(request)))
	waitBytes(t, "TotalRx", bw.TotalRx.Load, uint64(len(reply)))
	// Billable is the packet layer's business, not the socket's.
	if bw.BillableRx.Load() != 0 || bw.BillableTx.Load() != 0 {
		t.Fatalf("the socket layer must not touch billable: %d/%d", bw.BillableRx.Load(), bw.BillableTx.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sequence did not stop")
	}
}

// Without bandwidth accounting (a nil bw, as in tests and non-provider users of
// the package) nothing may change and nothing may panic.
func TestUdpSequenceWithoutBandwidthStillWorks(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dest := listener.LocalAddr().(*net.UDPAddr)
	go func() {
		buf := make([]byte, 2048)
		if n, from, err := listener.ReadFromUDP(buf); err == nil && n > 0 {
			listener.WriteToUDP([]byte("ok"), from)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotReply := make(chan struct{}, 1)
	seq := NewUdpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
		select {
		case gotReply <- struct{}{}:
		default:
		}
	}, DestinationId(NewId()), protocol.ProvideMode_Network, 4,
		net.ParseIP("127.0.0.1"), UDPPort(40002), dest.IP, UDPPort(dest.Port), DefaultUdpBufferSettings())
	go seq.Run()
	udp := &parsedUdp{}
	udp.payload = []byte("hi")
	if ok, _ := seq.send(&UdpSendItem{udp: udp, ipPacket: MessagePoolGet(2048)}, time.Second); !ok {
		t.Fatal("send not accepted")
	}
	select {
	case <-gotReply:
	case <-time.After(3 * time.Second):
		t.Fatal("no reply without bandwidth accounting")
	}
}

func TestTrackDirectCountsBothDirections(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	bw := &ProxyBandwidth{}
	c := trackDirect(a, bw)

	go b.Write([]byte("12345"))
	buf := make([]byte, 16)
	if n, err := c.Read(buf); err != nil || n != 5 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	go func() { tmp := make([]byte, 16); b.Read(tmp) }()
	if n, err := c.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if bw.TotalRx.Load() != 5 || bw.TotalTx.Load() != 3 {
		t.Fatalf("total rx/tx = %d/%d, want 5/3", bw.TotalRx.Load(), bw.TotalTx.Load())
	}
}

// A proxy dial already returns a trackedConn. Wrapping it again would count
// every byte twice.
func TestTrackDirectDoesNotDoubleCountAProxyConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	bw := &ProxyBandwidth{}
	proxied := &trackedConn{Conn: a, bw: bw}
	if got := trackDirect(proxied, bw); got != net.Conn(proxied) {
		t.Fatal("an already tracked conn must be returned as is")
	}
	go b.Write([]byte("xyz"))
	buf := make([]byte, 8)
	proxied.Read(buf)
	if bw.TotalRx.Load() != 3 {
		t.Fatalf("total rx = %d, want 3 (counted once)", bw.TotalRx.Load())
	}
}

func TestTrackDirectNilBandwidthIsANoOp(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if got := trackDirect(a, nil); got != net.Conn(a) {
		t.Fatal("no bandwidth accounting must leave the conn untouched")
	}
}

// TCP is where most real traffic is. This drives a real TcpSequence over a real
// loopback connection and checks that what the destination sends is counted as
// total received.
func TestTcpSequenceDirectSocketCountsTotalBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const greeting = "hello from the destination server, direct tcp"
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte(greeting))
		time.Sleep(2 * time.Second) // hold the conn open while the test looks
	}()
	dest := ln.Addr().(*net.TCPAddr)

	settings := DefaultTcpBufferSettings()
	settings.ConnectTimeout = 60 * time.Second
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seq := NewTcpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{},
		protocol.ProvideMode_Network,
		4,
		net.ParseIP("127.0.0.1").To4(), TCPPort(40003),
		dest.IP.To4(), TCPPort(dest.Port),
		settings,
	)
	bw := &ProxyBandwidth{}
	seq.bw = bw
	done := make(chan struct{})
	go func() { seq.Run(); close(done) }()

	seq.sendItems <- &TcpSendItem{tcp: &parsedTcp{syn: true}, ipPacket: MessagePoolGet(2048)}

	waitBytes(t, "TotalRx", bw.TotalRx.Load, uint64(len(greeting)))
	if bw.BillableRx.Load() != 0 || bw.BillableTx.Load() != 0 {
		t.Fatalf("the socket layer must not touch billable: %d/%d", bw.BillableRx.Load(), bw.BillableTx.Load())
	}

	// The acknowledged handshake has advanced sendSeq to the SYN's +1; a data
	// segment at that sequence is not a retransmit, so it is written to the
	// destination socket and counted as total sent.
	const outgoing = "request from the client"
	seq.sendItems <- &TcpSendItem{tcp: &parsedTcp{syn: false, seq: 1, payload: []byte(outgoing)}, ipPacket: MessagePoolGet(2048)}
	waitBytes(t, "TotalTx", bw.TotalTx.Load, uint64(len(outgoing)))
	if bw.BillableRx.Load() != 0 || bw.BillableTx.Load() != 0 {
		t.Fatalf("the socket layer must not touch billable: %d/%d", bw.BillableRx.Load(), bw.BillableTx.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sequence did not stop")
	}
}
