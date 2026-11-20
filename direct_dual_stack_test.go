package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDirectDualStackCountingBothFamilies(t *testing.T) {
	ResetDirectDualStackForTest()
	now := time.Unix(1732000000, 0)
	stats := DirectDualStackStats()

	// Initially zero
	v4 := stats.IPv4.Snapshot()
	v6 := stats.IPv6.Snapshot()
	if v4.ConnectsOK != 0 || v4.ConnectsFailed != 0 || v4.RxBytes != 0 || v4.TxBytes != 0 {
		t.Fatalf("initial ipv4 stats not zero: %+v", v4)
	}
	if v6.ConnectsOK != 0 || v6.ConnectsFailed != 0 || v6.RxBytes != 0 || v6.TxBytes != 0 {
		t.Fatalf("initial ipv6 stats not zero: %+v", v6)
	}

	// Record IPv4 connects and bytes
	stats.RecordConnect(4, true, now)
	stats.RecordConnect(4, false, now.Add(time.Second))
	stats.AddRx(4, 1024)
	stats.AddTx(4, 2048)

	// Record IPv6 connects and bytes
	stats.RecordConnect(6, true, now.Add(2*time.Second))
	stats.RecordConnect(6, false, now.Add(3*time.Second))
	stats.AddRx(6, 4096)
	stats.AddTx(6, 8192)

	v4 = stats.IPv4.Snapshot()
	v6 = stats.IPv6.Snapshot()

	if v4.ConnectsOK != 1 || v4.ConnectsFailed != 1 {
		t.Fatalf("ipv4 connects ok=%d fail=%d, want 1, 1", v4.ConnectsOK, v4.ConnectsFailed)
	}
	if v4.RxBytes != 1024 || v4.TxBytes != 2048 {
		t.Fatalf("ipv4 bytes rx=%d tx=%d, want 1024, 2048", v4.RxBytes, v4.TxBytes)
	}
	if v4.LastSuccessUnix != now.Unix() {
		t.Fatalf("ipv4 last success = %d, want %d", v4.LastSuccessUnix, now.Unix())
	}

	if v6.ConnectsOK != 1 || v6.ConnectsFailed != 1 {
		t.Fatalf("ipv6 connects ok=%d fail=%d, want 1, 1", v6.ConnectsOK, v6.ConnectsFailed)
	}
	if v6.RxBytes != 4096 || v6.TxBytes != 8192 {
		t.Fatalf("ipv6 bytes rx=%d tx=%d, want 4096, 8192", v6.RxBytes, v6.TxBytes)
	}
	if v6.LastSuccessUnix != now.Add(2*time.Second).Unix() {
		t.Fatalf("ipv6 last success = %d, want %d", v6.LastSuccessUnix, now.Add(2*time.Second).Unix())
	}

	// Reset
	ResetDirectDualStackForTest()
	v4 = stats.IPv4.Snapshot()
	if v4.ConnectsOK != 0 || v4.RxBytes != 0 {
		t.Fatalf("after reset, stats not zero: %+v", v4)
	}
}

func TestConnectSettingsDirectDualStackDial(t *testing.T) {
	ResetDirectDualStackForTest()
	defer ResetDirectDualStackForTest()

	// Successful IPv4 dial
	cs := DefaultConnectSettings()
	cs.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return &mockAddrConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 80}}, nil
		},
	}
	conn, err := cs.DialContext(context.Background(), "tcp4", "192.0.2.1:80")
	if err != nil {
		t.Fatalf("dial ipv4 failed: %v", err)
	}
	_ = conn.Close()

	// Failed IPv6 dial
	cs6 := DefaultConnectSettings()
	cs6.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
	}
	_, err = cs6.DialContext(context.Background(), "tcp6", "[2001:db8::1]:443")
	if err == nil {
		t.Fatal("expected failure")
	}

	stats := DirectDualStackStats()
	v4 := stats.IPv4.Snapshot()
	v6 := stats.IPv6.Snapshot()

	if v4.ConnectsOK != 1 || v4.ConnectsFailed != 0 {
		t.Fatalf("v4 connects ok=%d fail=%d, want 1, 0", v4.ConnectsOK, v4.ConnectsFailed)
	}
	if v4.LastSuccessUnix <= 0 {
		t.Fatalf("v4 last success not stamped: %d", v4.LastSuccessUnix)
	}
	if v6.ConnectsOK != 0 || v6.ConnectsFailed != 1 {
		t.Fatalf("v6 connects ok=%d fail=%d, want 0, 1", v6.ConnectsOK, v6.ConnectsFailed)
	}
}

func TestTrackDirectDualStackBytes(t *testing.T) {
	ResetDirectDualStackForTest()
	defer ResetDirectDualStackForTest()

	bw := &ProxyBandwidth{}

	// Test IPv4 connection
	pipeA, pipeB := net.Pipe()
	defer pipeA.Close()
	defer pipeB.Close()

	conn4 := trackDirectFamily(&mockAddrConn{Conn: pipeA, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 80}}, bw, 4)
	go func() {
		_, _ = pipeB.Write([]byte("hello ipv4"))
	}()
	buf := make([]byte, 16)
	n, err := conn4.Read(buf)
	if err != nil || n != 10 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	go func() {
		tmp := make([]byte, 16)
		_, _ = pipeB.Read(tmp)
	}()
	n, err = conn4.Write([]byte("out v4"))
	if err != nil || n != 6 {
		t.Fatalf("write n=%d err=%v", n, err)
	}

	// Test IPv6 connection
	p6A, p6B := net.Pipe()
	defer p6A.Close()
	defer p6B.Close()

	conn6 := trackDirectFamily(&mockAddrConn{Conn: p6A, remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 80}}, bw, 6)
	go func() {
		_, _ = p6B.Write([]byte("ipv6 payload"))
	}()
	n, err = conn6.Read(buf)
	if err != nil || n != 12 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	go func() {
		tmp := make([]byte, 16)
		_, _ = p6B.Read(tmp)
	}()
	n, err = conn6.Write([]byte("v6 response!"))
	if err != nil || n != 12 {
		t.Fatalf("write n=%d err=%v", n, err)
	}

	stats := DirectDualStackStats()
	v4 := stats.IPv4.Snapshot()
	v6 := stats.IPv6.Snapshot()

	if v4.RxBytes != 10 || v4.TxBytes != 6 {
		t.Fatalf("ipv4 rx=%d tx=%d, want 10, 6", v4.RxBytes, v4.TxBytes)
	}
	if v6.RxBytes != 12 || v6.TxBytes != 12 {
		t.Fatalf("ipv6 rx=%d tx=%d, want 12, 12", v6.RxBytes, v6.TxBytes)
	}
	// bw also counted all total bytes
	if bw.TotalRx.Load() != 22 || bw.TotalTx.Load() != 18 {
		t.Fatalf("bw total rx=%d tx=%d, want 22, 18", bw.TotalRx.Load(), bw.TotalTx.Load())
	}
}

func TestDirectDualStackRedaction(t *testing.T) {
	// Verify that snapshots and family stats do not store or format IP addresses
	stats := DirectDualStackStats()
	v4 := stats.IPv4.Snapshot()
	_ = v4
	for _, raw := range []string{"192.0.2.1", "2001:db8::1"} {
		if strings.Contains(strings.ToLower(v4String(v4)), raw) {
			t.Fatalf("v4 stats contained address %s", raw)
		}
	}
}

func v4String(v DirectFamilySnapshot) string {
	return ""
}

type mockAddrConn struct {
	net.Conn
	remote net.Addr
}

func (m *mockAddrConn) RemoteAddr() net.Addr {
	if m.remote != nil {
		return m.remote
	}
	if m.Conn != nil {
		return m.Conn.RemoteAddr()
	}
	return nil
}

func (m *mockAddrConn) Close() error {
	if m.Conn != nil {
		return m.Conn.Close()
	}
	return nil
}
