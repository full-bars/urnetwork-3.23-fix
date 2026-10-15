package connect

import (
	"context"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type pureAckHarness struct {
	seq       *TcpSequence
	cancel    context.CancelFunc
	peer      net.Conn
	mutex     sync.Mutex
	delivered []byte
	done      chan struct{}
}

func newPureAckHarness(t testing.TB) *pureAckHarness {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultTcpBufferSettings()
	settings.ConnectTimeout = 60 * time.Second
	local, peer := net.Pipe()
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return local, nil
		},
	}
	h := &pureAckHarness{cancel: cancel, peer: peer, done: make(chan struct{})}
	h.seq = NewTcpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{},
		protocol.ProvideMode_Network,
		4,
		net.ParseIP("127.0.0.1").To4(), TCPPort(40000),
		net.ParseIP("127.0.0.2").To4(), TCPPort(443),
		settings,
	)
	go func() {
		defer close(h.done)
		h.seq.Run()
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := peer.Read(buf)
			h.mutex.Lock()
			h.delivered = append(h.delivered, buf[:n]...)
			h.mutex.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		peer.Close()
		local.Close()
		<-h.done
	})
	return h
}

func (h *pureAckHarness) feed(t testing.TB, tcp *parsedTcp) {
	t.Helper()
	ok, err := h.seq.sendOrApplyPureAck(protocol.ProvideMode_Network, tcp, MessagePoolGet(64), -1)
	if !ok || err != nil {
		t.Fatalf("feed failed ok=%v err=%v", ok, err)
	}
}

func (h *pureAckHarness) waitReady(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.seq.mutex.Lock()
		ready := h.seq.directAckReady
		h.seq.mutex.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("sequence never became ready")
}

type pureAckState struct {
	SendSeq, ReceiveSeqAck, ReceiveWindow, TimestampRecent uint32
	Delivered                                              string
}

func (h *pureAckHarness) state() pureAckState {
	h.seq.mutex.Lock()
	s := pureAckState{
		SendSeq:         h.seq.sendSeq,
		ReceiveSeqAck:   h.seq.receiveSeqAck,
		ReceiveWindow:   h.seq.receiveWindowSize,
		TimestampRecent: h.seq.timestampRecent,
	}
	h.seq.mutex.Unlock()
	h.mutex.Lock()
	s.Delivered = string(h.delivered)
	h.mutex.Unlock()
	return s
}

// settle waits until the queue is empty and the state has been stable.
func (h *pureAckHarness) settle(t testing.TB, wantDelivered int) pureAckState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last pureAckState
	stable := 0
	for time.Now().Before(deadline) {
		s := h.state()
		if len(h.seq.sendItems) == 0 && len(s.Delivered) == wantDelivered && reflect.DeepEqual(s, last) {
			stable++
			if 20 <= stable {
				return s
			}
		} else {
			stable = 0
		}
		last = s
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state did not settle: %+v", last)
	return last
}

func pureAckSteps() []*parsedTcp {
	const isn = uint32(1000)
	return []*parsedTcp{
		{ack: true, seq: isn + 1, ackNumber: isn + 1, windowSize: 100},
		{ack: true, psh: true, seq: isn + 1, ackNumber: isn + 1, windowSize: 100, payload: []byte("hello")},
		{ack: true, seq: isn + 6, ackNumber: isn + 1, windowSize: 50},   // window update, same ack
		{ack: true, seq: isn + 6, ackNumber: isn + 1, windowSize: 50},   // duplicate ack
		{ack: true, seq: isn + 6, ackNumber: isn, windowSize: 7},        // older ack, ignored
		{ack: true, seq: 5000, ackNumber: isn + 9, windowSize: 9},       // wrong seq, dropped
		{ack: true, seq: isn + 6, ackNumber: isn + 40, windowSize: 0},   // zero window
		{ack: true, seq: isn + 6, ackNumber: isn + 40, windowSize: 200}, // window reopen
		{ack: true, psh: true, seq: isn + 6, ackNumber: isn + 40, windowSize: 200, payload: []byte("world")},
		{ack: true, seq: isn + 11, ackNumber: isn + 60, windowSize: 300},
		{ack: true, fin: true, seq: isn + 11, ackNumber: isn + 60, windowSize: 300},
		{ack: true, seq: isn + 12, ackNumber: isn + 99, windowSize: 999}, // ack after FIN, never applied
	}
}

func runPureAckScenario(t *testing.T, disableDirect bool) pureAckState {
	tcpPureAckDirectDisabled.Store(disableDirect)
	defer tcpPureAckDirectDisabled.Store(false)
	h := newPureAckHarness(t)
	h.feed(t, &parsedTcp{syn: true, seq: 1000, windowSize: 100})
	h.waitReady(t)
	for i, step := range pureAckSteps() {
		if i == len(pureAckSteps())-1 {
			// after FIN the sequence is torn down; both paths must refuse it
			ok, _ := h.seq.sendOrApplyPureAck(protocol.ProvideMode_Network, step, MessagePoolGet(64), 0)
			if ok {
				t.Fatalf("ack after FIN accepted (disableDirect=%v)", disableDirect)
			}
			continue
		}
		h.feed(t, step)
		if i == 1 || i == 8 || i == 10 {
			// payload goes through the socket writer; let it land so later
			// pure acks see the advanced sendSeq on both paths
			h.settle(t, map[int]int{1: 5, 8: 10, 10: 10}[i])
		}
	}
	return h.settle(t, 10)
}

func TestTcpPureAckDirectDifferential(t *testing.T) {
	old := runPureAckScenario(t, true)
	direct := runPureAckScenario(t, false)
	if !reflect.DeepEqual(old, direct) {
		t.Fatalf("state diverged\nqueued: %+v\ndirect: %+v", old, direct)
	}
	if old.ReceiveSeqAck != 1060 || old.ReceiveWindow != 300<<0 {
		t.Fatalf("unexpected final state %+v", old)
	}
}

// The direct path must actually be taken for an established pure ACK.
func TestTcpPureAckDirectBypassesQueue(t *testing.T) {
	h := newPureAckHarness(t)
	h.feed(t, &parsedTcp{syn: true, seq: 1000, windowSize: 100})
	h.waitReady(t)
	if !h.seq.applyPureAckDirect(&parsedTcp{ack: true, seq: 1001, ackNumber: 1001, windowSize: 77}) {
		t.Fatal("established pure ack was not applied directly")
	}
	if s := h.state(); s.ReceiveSeqAck != 1001 || s.ReceiveWindow != 77 {
		t.Fatalf("state %+v", s)
	}
	// wrong seq must fall back to the queue
	if h.seq.applyPureAckDirect(&parsedTcp{ack: true, seq: 4000, ackNumber: 1002, windowSize: 1}) {
		t.Fatal("out-of-order pure ack applied directly")
	}
}

// Before the handshake completes, pure acks keep using the ordered queue.
func TestTcpPureAckDirectNotReadyBeforeSyn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq := NewTcpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{}, protocol.ProvideMode_Network, 4,
		net.ParseIP("127.0.0.1").To4(), 1, net.ParseIP("127.0.0.2").To4(), 2, DefaultTcpBufferSettings())
	if seq.applyPureAckDirect(&parsedTcp{ack: true}) {
		t.Fatal("applied before established")
	}
}

func TestTcpPureAckDirectWakesBlockedReader(t *testing.T) {
	h := newPureAckHarness(t)
	h.feed(t, &parsedTcp{syn: true, seq: 1000, windowSize: 1})
	h.waitReady(t)
	cond := make(chan struct{})
	go func() {
		h.seq.mutex.Lock()
		defer h.seq.mutex.Unlock()
		for h.seq.receiveWindowSize < 500 {
			h.seq.receiveAckCond.Wait()
		}
		close(cond)
	}()
	time.Sleep(20 * time.Millisecond)
	h.feed(t, &parsedTcp{ack: true, seq: 1001, ackNumber: 1001, windowSize: 600})
	select {
	case <-cond:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
}

func pureAckParsed() *parsedTcp {
	return &parsedTcp{ack: true, seq: 1001, ackNumber: 1001, windowSize: 100}
}

func TestTcpPureAckDirectAllocs(t *testing.T) {
	h := newPureAckHarness(t)
	h.feed(t, &parsedTcp{syn: true, seq: 1000, windowSize: 100})
	h.waitReady(t)
	tcp := pureAckParsed()
	measure := func(disable bool) float64 {
		tcpPureAckDirectDisabled.Store(disable)
		defer tcpPureAckDirectDisabled.Store(false)
		return testing.AllocsPerRun(200, func() {
			ok, _ := h.seq.sendOrApplyPureAck(protocol.ProvideMode_Network, tcp, MessagePoolGet(64), -1)
			if !ok {
				t.Fatal("not accepted")
			}
			if disable {
				for len(h.seq.sendItems) != 0 {
					time.Sleep(10 * time.Microsecond)
				}
			}
		})
	}
	queued, direct := measure(true), measure(false)
	t.Logf("allocs/op queued=%v direct=%v", queued, direct)
	if direct != 0 || queued <= direct {
		t.Fatalf("direct path should not allocate: queued=%v direct=%v", queued, direct)
	}
}

func benchPureAck(b *testing.B, disable bool) {
	tcpPureAckDirectDisabled.Store(disable)
	defer tcpPureAckDirectDisabled.Store(false)
	h := newPureAckHarness(b)
	h.feed(b, &parsedTcp{syn: true, seq: 1000, windowSize: 100})
	h.waitReady(b)
	tcp := pureAckParsed()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.seq.sendOrApplyPureAck(protocol.ProvideMode_Network, tcp, MessagePoolGet(64), -1)
	}
}

func BenchmarkTcpPureAckQueued(b *testing.B) { benchPureAck(b, true) }
func BenchmarkTcpPureAckDirect(b *testing.B) { benchPureAck(b, false) }

var _ = io.EOF
