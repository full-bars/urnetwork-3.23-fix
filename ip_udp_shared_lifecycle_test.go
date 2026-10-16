package connect

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The shared-socket lifecycle collapses the per-flow read/write machinery
// (two goroutines and the timers per UDP flow) into the poller's shard
// workers and one buffer-level idle scan. These tests are the evidence for
// that port: one measures the goroutine budget for a fixed flow count in
// both modes, the other proves the shared path carries traffic end to end.

func awaitGoroutinesAtMost(t *testing.T, limit int, what string) int {
	t.Helper()
	for i := 0; i < 200000; i++ {
		if n := runtime.NumGoroutine(); n <= limit {
			return n
		}
		runtime.Gosched()
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Logf("goroutine dump:\n%s", buf[:n])
	t.Fatalf("%s: goroutines never returned to %d (have %d)", what, limit, runtime.NumGoroutine())
	return 0
}

func awaitGoroutinesStable(t *testing.T, what string) int {
	t.Helper()
	stable := runtime.NumGoroutine()
	for rounds := 0; rounds < 100; {
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == stable {
			rounds++
			continue
		}
		stable = n
		rounds = 0
	}
	return stable
}

// measureFlows brings up flowCount UDP flows against a local sink in the
// given mode and returns the settled goroutine count while they are live.
func measureFlows(t *testing.T, shardCount int, flowCount int) int {
	t.Helper()
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	settings := DefaultUdpBufferSettings()
	settings.SocketReadShardCount = shardCount
	settings.IdleTimeout = time.Hour // no idle reaping while counting

	buffer := NewUdp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings, nil)

	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		cancel()
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()
	sinkPort := UDPPort(sink.LocalAddr().(*net.UDPAddr).Port)

	for i := 0; i < flowCount; i++ {
		udp := &parsedUdp{
			sourceIp:        net.ParseIP("127.0.0.1").To4(),
			destinationIp:   net.ParseIP("127.0.0.1").To4(),
			sourcePort:      UDPPort(40000 + i),
			destinationPort: sinkPort,
			payload:         []byte("x"),
		}
		packet := MessagePoolGet(2048)
		if success, err := buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, time.Second, packet); err != nil || !success {
			// send already returned the packet on its failure paths
			cancel()
			t.Fatalf("flow %d send: success=%v err=%v", i, success, err)
		}
	}

	// The per-flow fallback owns about three goroutines per flow; they spawn
	// asynchronously as each Run progresses, so a premature calm must not be
	// mistaken for the steady state. Wait for the expected floor first in the
	// fallback mode (the shared mode legitimately holds far fewer).
	if shardCount == 0 {
		want := baseline + flowCount*2
		for i := 0; i < 200000 && runtime.NumGoroutine() < want; i += 1 {
			runtime.Gosched()
		}
	}
	live := awaitGoroutinesStable(t, "flows live")

	cancel()
	// +4 tolerates helpers the package starts lazily during the measure (the
	// pool-statistics loop among them); the deltas this test asserts are far
	// larger than that drift.
	awaitGoroutinesAtMost(t, baseline+4, "flows torn down")
	return live - baseline
}

// The before/after evidence: 32 flows in the per-flow fallback versus the
// shared-socket lifecycle with four shards. The shared mode owns no per-flow
// reader or writer goroutines, so it must hold a materially smaller budget.
func TestUdpSharedSocketLifecycleGoroutineBudget(t *testing.T) {
	const flowCount = 32
	fallback := measureFlows(t, 0, flowCount)
	shared := measureFlows(t, 4, flowCount)
	t.Logf("goroutines for %d flows: per-flow fallback=%d, shared lifecycle=%d", flowCount, fallback, shared)
	if shared >= fallback {
		t.Fatalf("the shared lifecycle must reduce the goroutine budget: fallback=%d shared=%d", fallback, shared)
	}
	// our per-flow path owns a reader and a writer goroutine on top of the
	// sequence itself; the shared path collapses both into the shards. Half
	// the flow count is a conservative floor for the saving.
	if shared > fallback-flowCount/2 {
		t.Fatalf("the shared lifecycle saved too little: fallback=%d shared=%d", fallback, shared)
	}
}

// End to end on the shared path: the flow's payload reaches the sink through
// the shared socket, and the sink's reply is delivered to the receive
// callback by the poller's shard worker.
func TestUdpSharedSocketLifecycleCarriesTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan []byte, 4)
	settings := DefaultUdpBufferSettings()
	settings.SocketReadShardCount = 2
	settings.IdleTimeout = time.Hour

	buffer := NewUdp4Buffer(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		payload := append([]byte(nil), packet...)
		select {
		case received <- payload:
		default:
		}
	}, settings, nil)

	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()
	sinkPort := UDPPort(sink.LocalAddr().(*net.UDPAddr).Port)

	udp := &parsedUdp{
		sourceIp:        net.ParseIP("127.0.0.1").To4(),
		destinationIp:   net.ParseIP("127.0.0.1").To4(),
		sourcePort:      UDPPort(41000),
		destinationPort: sinkPort,
		payload:         []byte("outbound-through-shared-socket"),
	}
	packet := MessagePoolGet(2048)
	copy(packet, udp.payload)
	if success, err := buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, time.Second, packet); err != nil || !success {
		t.Fatalf("send: success=%v err=%v", success, err)
	}

	// the sink must see the payload
	sink.SetReadDeadline(time.Now().Add(10 * time.Second))
	readBuf := make([]byte, 2048)
	n, from, err := sink.ReadFromUDP(readBuf)
	if err != nil {
		t.Fatalf("sink read: %v", err)
	}
	if got := string(readBuf[:n]); got != "outbound-through-shared-socket" {
		t.Fatalf("sink payload = %q", got)
	}

	// the reply must come back through the poller's drain to the callback
	if _, err := sink.WriteToUDP([]byte("reply-through-poller"), from); err != nil {
		t.Fatalf("sink write: %v", err)
	}

	select {
	case got := <-received:
		if !strings.Contains(string(got), "reply-through-poller") {
			t.Fatalf("receive callback packet does not carry the reply: %q", string(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reply never reached the receive callback through the shared poller")
	}
}

// The same traffic shape through the per-flow fallback, as the control: it
// passes today, and it must keep passing after the port.
func TestUdpPerFlowFallbackCarriesTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan []byte, 4)
	settings := DefaultUdpBufferSettings()
	settings.SocketReadShardCount = 0
	settings.IdleTimeout = time.Hour

	buffer := NewUdp4Buffer(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		payload := append([]byte(nil), packet...)
		select {
		case received <- payload:
		default:
		}
	}, settings, nil)

	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()
	sinkPort := UDPPort(sink.LocalAddr().(*net.UDPAddr).Port)

	udp := &parsedUdp{
		sourceIp:        net.ParseIP("127.0.0.1").To4(),
		destinationIp:   net.ParseIP("127.0.0.1").To4(),
		sourcePort:      UDPPort(42000),
		destinationPort: sinkPort,
		payload:         []byte("outbound-per-flow"),
	}
	packet := MessagePoolGet(2048)
	copy(packet, udp.payload)
	if success, err := buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, time.Second, packet); err != nil || !success {
		t.Fatalf("send: success=%v err=%v", success, err)
	}

	sink.SetReadDeadline(time.Now().Add(10 * time.Second))
	readBuf := make([]byte, 2048)
	n, from, err := sink.ReadFromUDP(readBuf)
	if err != nil {
		t.Fatalf("sink read: %v", err)
	}
	if got := string(readBuf[:n]); got != "outbound-per-flow" {
		t.Fatalf("sink payload = %q", got)
	}
	if _, err := sink.WriteToUDP([]byte("reply-per-flow"), from); err != nil {
		t.Fatalf("sink write: %v", err)
	}
	select {
	case got := <-received:
		if !strings.Contains(string(got), "reply-per-flow") {
			t.Fatalf("receive callback packet does not carry the reply: %q", string(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reply never reached the receive callback in the fallback mode")
	}
}
