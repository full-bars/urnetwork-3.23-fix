//go:build unix || windows

// Fork-side coverage for provider egress realism. The upstream test drives the
// provider reliable-ingress fixtures (runReliableIngressFixtureDelivery,
// afterSocketOpenForTest, afterUpstreamConnectForTest, testUdpFlowIps) that this
// tree does not carry, so the decisions are exercised directly here instead:
// target construction, the dial hook against a real loopback socket, the
// post-connect fallback, and one whole NAT flow per transport.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// providerEgressTestTtl reads the real socket option, not the target's intent.
func providerEgressTestTtl(t *testing.T, conn net.Conn, ipv6 bool) int {
	t.Helper()
	socket, ok := conn.(syscall.Conn)
	if !ok {
		t.Fatalf("%T is not a syscall.Conn", conn)
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ttl int
	var optionErr error
	if err := raw.Control(func(fd uintptr) { ttl, optionErr = providerSocketTtl(fd, ipv6) }); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil {
		t.Fatal(optionErr)
	}
	return ttl
}

// Native mode and a zero TTL both build no target, so nothing reaches the
// socket and no flow is counted.
func TestProviderEgressTargetNativeIsZero(t *testing.T) {
	var counters egressRealismCounters
	for _, mode := range []EgressTtlMode{"", EgressTtlModeNative} {
		target := newProviderEgressTarget(mode, 37, ipProtocolNumberTcp, &counters)
		if target.mode != "" || target.ttl != 0 || target.counters != nil || target.setTtlForTest != nil {
			t.Fatalf("mode %q built a live target: %+v", mode, target)
		}
	}
	if counters.tcpFlowCount.Load() != 0 || counters.mirrorFlowCount.Load() != 0 {
		t.Fatalf("native mode counted a flow")
	}
	ctx, dial := (&providerEgressTarget{mode: EgressTtlModeMirror}).dialContext(context.Background())
	if dial != nil || ctx.Value(providerEgressDialKey{}) != nil {
		t.Fatalf("zero ttl dialed a target")
	}
}

// Each protocol is counted on its own flow axis and a zero TTL is retained as
// an invalid flow rather than silently rewritten to a default.
func TestProviderEgressTargetCountsFlowsAndRejectsZeroTtl(t *testing.T) {
	var counters egressRealismCounters
	shadow := newProviderEgressTarget(EgressTtlModeShadow, 37, ipProtocolNumberTcp, &counters)
	mirror := newProviderEgressTarget(EgressTtlModeMirror, 41, ipProtocolNumberUdp, &counters)
	newProviderEgressTarget(EgressTtlModeMirror, 0, ipProtocolNumberUdp, &counters)
	if shadow.mode != EgressTtlModeShadow || shadow.ttl != 37 || mirror.ttl != 41 {
		t.Fatalf("target did not retain mode and ttl: shadow=%+v mirror=%+v", shadow, mirror)
	}
	if counters.tcpFlowCount.Load() != 1 || counters.udpFlowCount.Load() != 2 {
		t.Fatalf("flow counts tcp=%d udp=%d", counters.tcpFlowCount.Load(), counters.udpFlowCount.Load())
	}
	if counters.shadowFlowCount.Load() != 1 || counters.mirrorFlowCount.Load() != 2 {
		t.Fatalf("mode counts shadow=%d mirror=%d", counters.shadowFlowCount.Load(), counters.mirrorFlowCount.Load())
	}
	if counters.invalidTtlFlowCount.Load() != 1 {
		t.Fatalf("invalid ttl flows = %d", counters.invalidTtlFlowCount.Load())
	}
}

// The pre-connect hook must set the option on the real socket, count it once,
// and leave the post-connect subset to the paths that bypass the hook.
func TestProviderEgressMirrorsTtlOnRealSocket(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no tcp4 loopback: %v", err)
	}
	defer listener.Close()
	go func() {
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
		}
	}()

	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, dial := target.dialContext(context.Background())
	if dial == nil {
		t.Fatal("mirror target did not instrument the dial context")
	}
	if ctx.Value(providerEgressDialKey{}) != dial {
		t.Fatal("dial handle is not the one carried by the context")
	}
	socket, err := DefaultConnectSettings().DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if ttl := providerEgressTestTtl(t, socket, false); ttl != 37 {
		t.Fatalf("socket TTL = %d, want 37", ttl)
	}
	dial.afterConnect(socket)
	if counters.preConnectSocketCount.Load() != 1 || counters.postConnectSocketCount.Load() != 0 {
		t.Fatalf("pre=%d post=%d", counters.preConnectSocketCount.Load(), counters.postConnectSocketCount.Load())
	}
	if counters.ttlApplyCount.Load() != 1 || counters.ttlApplyErrorCount.Load() != 0 {
		t.Fatalf("apply=%d applyError=%d", counters.ttlApplyCount.Load(), counters.ttlApplyErrorCount.Load())
	}
	if total := counters.ttlMatchCount.Load() + counters.ttlMismatchCount.Load(); total != 1 {
		t.Fatalf("native TTL was not compared exactly once: %d", total)
	}
	if counters.socketUnavailableCount.Load() != 0 {
		t.Fatalf("socket unavailable = %d", counters.socketUnavailableCount.Load())
	}
}

// An opaque host dial never runs the hook, so the post-connect subset observes
// the socket it actually owns, including an unsupported conn type.
func TestProviderEgressAfterConnectCoversOpaqueDials(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberUdp, &counters)
	_, dial := target.dialContext(context.Background())

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	dial.afterConnect(client)
	if counters.postConnectSocketCount.Load() != 1 || counters.socketUnavailableCount.Load() != 1 {
		t.Fatalf("opaque conn post=%d unavailable=%d",
			counters.postConnectSocketCount.Load(), counters.socketUnavailableCount.Load())
	}

	// A control hook that already ran owns the observation; the fallback must
	// not double count it.
	var second egressRealismCounters
	observed := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberUdp, &second)
	_, observedDial := observed.dialContext(context.Background())
	observedDial.controlObserved.Store(true)
	observedDial.afterConnect(client)
	if second.postConnectSocketCount.Load() != 0 {
		t.Fatalf("observed control was counted again")
	}

	var nilDial *providerEgressDial
	nilDial.afterConnect(client)
}

// Control errors from earlier in the chain stay authoritative, and a refused
// option is a diagnostic rather than a dial failure.
type providerEgressTestRawConn struct {
	controls int
	fd       uintptr
}

func (self *providerEgressTestRawConn) Control(fn func(uintptr)) error {
	self.controls++
	if self.fd == 0 {
		return syscall.EBADF
	}
	fn(self.fd)
	return nil
}

func (self *providerEgressTestRawConn) Read(func(uintptr) bool) error  { return syscall.EBADF }
func (self *providerEgressTestRawConn) Write(func(uintptr) bool) error { return syscall.EBADF }

func TestProviderEgressControlPreservesEarlierFailures(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, _ := target.dialContext(context.Background())

	raw := &providerEgressTestRawConn{}
	dialer := DefaultConnectSettings().NetDialer()
	if err := dialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); err != nil {
		t.Fatalf("raw failure aborted the dial: %v", err)
	}
	if raw.controls != 1 || counters.socketUnavailableCount.Load() != 1 {
		t.Fatalf("controls=%d unavailable=%d", raw.controls, counters.socketUnavailableCount.Load())
	}

	wantErr := errors.New("existing socket control failure")
	dialer.Control = func(string, string, syscall.RawConn) error { return wantErr }
	if err := dialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); !errors.Is(err, wantErr) {
		t.Fatalf("plain control failure lost: %v", err)
	}
	if raw.controls != 1 {
		t.Fatalf("the option ran after the control failed: %d", raw.controls)
	}

	contextDialer := providerEgressDialer(&net.Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error {
		return wantErr
	}})
	if err := contextDialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); !errors.Is(err, wantErr) {
		t.Fatalf("context control failure lost: %v", err)
	}
}

// A readable descriptor can still refuse getsockopt/setsockopt. Both failures
// count separately and the hook stays successful.
func TestProviderEgressSocketOptionFailuresAreBestEffort(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, _ := target.dialContext(context.Background())
	raw := &providerEgressTestRawConn{fd: ^uintptr(0)}
	if err := DefaultConnectSettings().NetDialer().ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); err != nil {
		t.Fatalf("option failure not best effort: %v", err)
	}
	if counters.ttlReadErrorCount.Load() != 1 || counters.ttlApplyErrorCount.Load() != 1 {
		t.Fatalf("read=%d apply=%d", counters.ttlReadErrorCount.Load(), counters.ttlApplyErrorCount.Load())
	}
	if counters.socketUnavailableCount.Load() != 0 {
		t.Fatalf("a refused option was miscounted as an unavailable socket")
	}
}

// A refused setTtl is counted per socket and the flow keeps forwarding.
func TestProviderEgressRefusedSetTtlStillForwards(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, _ := target.dialContext(context.Background())
	target.setTtlForTest = func(uintptr, bool, int) error { return syscall.EPERM }
	raw := &providerEgressTestRawConn{fd: 1}
	if err := DefaultConnectSettings().NetDialer().ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); err != nil {
		t.Fatal(err)
	}
	if counters.ttlApplyErrorCount.Load() != 1 || counters.ttlApplyCount.Load() != 0 {
		t.Fatalf("apply=%d applyError=%d", counters.ttlApplyCount.Load(), counters.ttlApplyErrorCount.Load())
	}
}

// Shadow mode reads the socket and never writes it, so a shadow flow reports
// a comparison and no apply.
func TestProviderEgressShadowNeverWrites(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no tcp4 loopback: %v", err)
	}
	defer listener.Close()
	go func() {
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
		}
	}()
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeShadow, 37, ipProtocolNumberTcp, &counters)
	ctx, dial := target.dialContext(context.Background())
	socket, err := DefaultConnectSettings().DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	dial.afterConnect(socket)
	if counters.ttlApplyCount.Load() != 0 || counters.ttlApplyErrorCount.Load() != 0 {
		t.Fatalf("shadow wrote the socket: apply=%d err=%d", counters.ttlApplyCount.Load(), counters.ttlApplyErrorCount.Load())
	}
	if total := counters.ttlMatchCount.Load() + counters.ttlMismatchCount.Load(); total != 1 {
		t.Fatalf("shadow did not compare the native TTL: %d", total)
	}
}

// The NAT aggregate reports every counter the owning NAT holds.
func TestLocalUserNatEgressRealismStatsAggregate(t *testing.T) {
	nat := &LocalUserNat{}
	nat.egressRealism.tcpFlowCount.Add(2)
	nat.egressRealism.udpFlowCount.Add(3)
	nat.egressRealism.shadowFlowCount.Add(4)
	nat.egressRealism.mirrorFlowCount.Add(1)
	nat.egressRealism.invalidTtlFlowCount.Add(5)
	nat.egressRealism.preConnectSocketCount.Add(6)
	nat.egressRealism.postConnectSocketCount.Add(7)
	nat.egressRealism.socketUnavailableCount.Add(8)
	nat.egressRealism.ttlMatchCount.Add(9)
	nat.egressRealism.ttlMismatchCount.Add(10)
	nat.egressRealism.ttlReadErrorCount.Add(11)
	nat.egressRealism.ttlApplyCount.Add(12)
	nat.egressRealism.ttlApplyErrorCount.Add(13)
	nat.egressRealism.keepAliveApplyErrorCount.Add(14)
	stats := nat.EgressRealismStats()
	want := EgressRealismStats{
		TcpFlowCount: 2, UdpFlowCount: 3, ShadowFlowCount: 4, MirrorFlowCount: 1,
		InvalidTtlFlowCount: 5, PreConnectSocketCount: 6, PostConnectSocketCount: 7,
		SocketUnavailableCount: 8, TtlMatchCount: 9, TtlMismatchCount: 10,
		TtlReadErrorCount: 11, TtlApplyCount: 12, TtlApplyErrorCount: 13,
		KeepAliveApplyErrorCount: 14,
	}
	if stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
}

// A whole NAT flow: the packet's TTL has to survive dispatch, the udp/tcp
// buffer, the sequence and the dial hook to reach the real socket. The SYN
// retransmission reuses the flow and must not retarget the established socket.
func TestProviderEgressTcpNatFlowMirrorsPacketTtl(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no tcp4 loopback: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultLocalUserNatSettings()
	settings.Log = NewNoopLogger()
	settings.TcpBufferSettings.EgressTtlMode = EgressTtlModeMirror
	base := settings.TcpBufferSettings.ConnectSettings
	sockets := make(chan net.Conn, 4)
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			socket, err := base.DialContext(ctx, network, address)
			if err == nil {
				sockets <- socket
			}
			return socket, err
		},
	}
	nat := NewLocalUserNat(ctx, "provider egress test", nil, settings)
	defer nat.Close()

	path := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.0.0.7").To4(),
		SourcePort:      42000,
		DestinationIp:   net.ParseIP("127.0.0.1").To4(),
		DestinationPort: listener.Addr().(*net.TCPAddr).Port,
	}
	source := SourceId(NewId())
	packet := egressTestPacketWithTtl(path, 37)
	if !nat.SendPacket(source, protocol.ProvideMode_Network, packet, 5*time.Second) {
		t.Fatal("SYN did not enter the NAT")
	}
	var socket net.Conn
	select {
	case socket = <-sockets:
	case <-time.After(15 * time.Second):
		t.Fatal("the flow never dialed upstream")
	}
	defer socket.Close()
	if ttl := providerEgressTestTtl(t, socket, false); ttl != 37 {
		t.Fatalf("socket TTL = %d, want 37", ttl)
	}
	stats := nat.EgressRealismStats()
	if stats.TcpFlowCount != 1 || stats.MirrorFlowCount != 1 || stats.TtlApplyCount != 1 {
		t.Fatalf("first SYN stats = %+v", stats)
	}

	retransmit := egressTestPacketWithTtl(path, 99)
	if !nat.SendPacket(source, protocol.ProvideMode_Network, retransmit, 5*time.Second) {
		t.Fatal("SYN retransmission did not enter the NAT")
	}
	if ttl := providerEgressTestTtl(t, socket, false); ttl != 37 {
		t.Fatalf("retransmission retargeted the socket: TTL=%d", ttl)
	}
	if after := nat.EgressRealismStats(); after != stats {
		t.Fatalf("retransmission changed the counters: %+v", after)
	}
}

// egressTestPacketWithTtl builds the client's SYN and stamps the header TTL the
// source chose, repairing the v4 checksum the way the kernel would.
func egressTestPacketWithTtl(path *IpPath, ttl uint8) []byte {
	packet := ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil)
	packet[8] = ttl
	packet[10], packet[11] = 0, 0
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[:20])))
	return MessagePoolCopy(packet)
}
