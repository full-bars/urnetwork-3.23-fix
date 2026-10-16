package connect

import (
	"context"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Fixture ported from perf/pure-ack-direct's harness: a TcpSequence over a
// net.Pipe pair with a peer reader, so delivered bytes and sequence state can
// be observed. The pure ack path is exercised through the production entry
// sendOrApplyPureAck.

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

func (h *pureAckHarness) waitEstablished(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.seq.mutex.Lock()
		established := h.seq.established
		h.seq.mutex.Unlock()
		if established {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("sequence never became established")
}

type pureAckState struct {
	SendSeq, ReceiveSeqAck, ReceiveWindow, WindowEnd uint32
	Delivered                                        string
}

func (h *pureAckHarness) state() pureAckState {
	h.seq.mutex.Lock()
	s := pureAckState{
		SendSeq:       h.seq.sendSeq,
		ReceiveSeqAck: h.seq.receiveSeqAck,
		ReceiveWindow: h.seq.receiveWindowSize,
		WindowEnd:     h.seq.receiveWindowEnd,
	}
	h.seq.mutex.Unlock()
	h.mutex.Lock()
	s.Delivered = string(h.delivered)
	h.mutex.Unlock()
	return s
}

// semanticState is the shared-semantics view for old-vs-new comparisons:
// receiveWindowEnd exists only while the monotonic model is on, so the
// differential compares the fields both models maintain.
type semanticState struct {
	SendSeq, ReceiveSeqAck, ReceiveWindow uint32
	Delivered                             string
}

func (h *pureAckHarness) semantic() semanticState {
	s := h.state()
	return semanticState{SendSeq: s.SendSeq, ReceiveSeqAck: s.ReceiveSeqAck, ReceiveWindow: s.ReceiveWindow, Delivered: s.Delivered}
}

// settle waits until the queue is empty and the state has been stable.
func (h *pureAckHarness) settle(t testing.TB, wantDelivered int) pureAckState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last pureAckState
	stable := 0
	for time.Now().Before(deadline) {
		s := h.state()
		h.seq.sendMutex.Lock()
		queued := len(h.seq.sendItems)
		h.seq.sendMutex.Unlock()
		if queued == 0 && len(s.Delivered) == wantDelivered && reflect.DeepEqual(s, last) {
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

// withTcpAckDirect pins the kill switch for one test run (package-internal
// override; the environment default is untouched).
func withTcpAckDirect(t *testing.T, enabled bool) {
	t.Helper()
	previous := tcpAckDirectOverride.Load()
	if enabled {
		tcpAckDirectOverride.Store(tcpAckDirectOn)
	} else {
		tcpAckDirectOverride.Store(tcpAckDirectOff)
	}
	t.Cleanup(func() {
		tcpAckDirectOverride.Store(previous)
	})
}

func resetTcpAckCounters() (direct, queued, stale int64) {
	direct, queued, stale = tcpAckDirectAppliedCount.Swap(0), tcpAckQueuedCount.Swap(0), tcpAckStaleIgnoredCount.Swap(0)
	return
}

const pureAckIsn = uint32(1000)

// advanceReturn simulates bytes returned on our side of the flow: the peer's
// ackNumber may only ack bytes actually emitted (receiveSeq), so tests that
// exercise ack numbers beyond the SYN must advance it first.
func (h *pureAckHarness) advanceReturn(t testing.TB, n uint32) {
	t.Helper()
	h.seq.mutex.Lock()
	h.seq.receiveSeq += n
	h.seq.mutex.Unlock()
}

func establishedHarness(t *testing.T) *pureAckHarness {
	h := newPureAckHarness(t)
	h.feed(t, &parsedTcp{syn: true, seq: pureAckIsn, windowSize: 100})
	h.waitEstablished(t)
	// the peer may ack our return side; say we returned plenty
	h.advanceReturn(t, 4096)
	return h
}

// A reordered older ack (lower ackNumber, or the same ackNumber with a
// smaller window) can never shrink the advertised edge or move the ack back.
func TestTcpAckMonotonicEdgeNeverShrinks(t *testing.T) {
	withTcpAckDirect(t, true)
	resetTcpAckCounters()
	h := establishedHarness(t)
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 40, windowSize: 600})
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 40, windowSize: 100}) // same edge, smaller window
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 2, ackNumber: pureAckIsn + 30, windowSize: 50})  // older ack
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 2, ackNumber: pureAckIsn + 35, windowSize: 999}) // still older than edge? no: below the edge's ack
	s := h.settle(t, 0)
	if s.ReceiveSeqAck != pureAckIsn+40 || s.WindowEnd != pureAckIsn+40+600 {
		t.Fatalf("edge moved backwards: %+v", s)
	}
	if s.ReceiveWindow != 600 {
		t.Fatalf("window shrank: %+v", s)
	}
	_, _, stale := resetTcpAckCounters()
	if stale == 0 {
		t.Fatal("the stale updates were not counted")
	}
}

// Zero window closes at the existing edge and a later reopen grows it.
func TestTcpAckZeroWindowClosesAndReopens(t *testing.T) {
	withTcpAckDirect(t, true)
	h := establishedHarness(t)
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 100, windowSize: 0})
	s := h.settle(t, 0)
	if s.ReceiveWindow != 0 || s.WindowEnd != pureAckIsn+100 {
		t.Fatalf("zero window: %+v", s)
	}
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 100, windowSize: 500})
	s = h.settle(t, 0)
	if s.ReceiveWindow != 500 || s.WindowEnd != pureAckIsn+600 {
		t.Fatalf("reopen: %+v", s)
	}
}

// The serial-number comparisons must treat acks across the 2^32 boundary
// correctly in both directions.
func TestTcpAckWraparound(t *testing.T) {
	withTcpAckDirect(t, true)
	h := newPureAckHarness(t)
	nearWrap := uint32(0xFFFFFF00)
	h.feed(t, &parsedTcp{syn: true, seq: nearWrap, windowSize: 100})
	h.waitEstablished(t)
	// logically ahead of the wrap point: accepted as newer
	h.feed(t, &parsedTcp{ack: true, seq: nearWrap + 1, ackNumber: nearWrap + 200, windowSize: 100})
	s := h.settle(t, 0)
	if s.ReceiveSeqAck != nearWrap+200 {
		t.Fatalf("crossover ack refused: %+v", s)
	}
	// logically behind (older): refused, edge kept
	h.feed(t, &parsedTcp{ack: true, seq: nearWrap + 1, ackNumber: nearWrap + 100, windowSize: 100})
	s = h.settle(t, 0)
	if s.ReceiveSeqAck != nearWrap+200 {
		t.Fatalf("older ack applied across the wrap: %+v", s)
	}
}

// After the run loop consumes a FIN, later pure acks must not be applied
// directly (the loop exits and would not have applied them either).
func TestTcpAckAfterFinStopsTheDirectPath(t *testing.T) {
	withTcpAckDirect(t, true)
	h := establishedHarness(t)
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100})
	h.settle(t, 0)
	h.feed(t, &parsedTcp{ack: true, fin: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100})
	// wait for the loop to consume the FIN
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.seq.mutex.Lock()
		stopped := h.seq.ackDirectStopped
		h.seq.mutex.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	resetTcpAckCounters()
	before := h.state()
	// the run loop cancels the sequence as it exits, so the queue is dead;
	// the property to pin is that the direct entry refuses (the packet would
	// have been dropped either way)
	if h.seq.applyEstablishedPureAck(&parsedTcp{ack: true, seq: pureAckIsn + 2, ackNumber: pureAckIsn + 50, windowSize: 100}) {
		t.Fatal("a pure ack after the consumed FIN was applied directly")
	}
	after := h.state()
	if tcpAckDirectAppliedCount.Load() != 0 {
		t.Fatal("direct counter moved after the consumed FIN")
	}
	if after.ReceiveSeqAck != before.ReceiveSeqAck || after.ReceiveWindow != before.ReceiveWindow {
		t.Fatalf("ack state moved after the FIN: %+v -> %+v", before, after)
	}
}

// A pure ack before the SYN is established must never apply directly; it
// takes the queue (and the queue drops it before the handshake, unchanged).
func TestTcpAckBeforeSynNeverAppliesDirectly(t *testing.T) {
	withTcpAckDirect(t, true)
	directBefore, queuedBefore, _ := resetTcpAckCounters()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq := NewTcpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{}, protocol.ProvideMode_Network, 4,
		net.ParseIP("127.0.0.1").To4(), 1, net.ParseIP("127.0.0.2").To4(), 2, DefaultTcpBufferSettings())
	if seq.applyEstablishedPureAck(&parsedTcp{ack: true, seq: 1001, ackNumber: 1001, windowSize: 100}) {
		t.Fatal("applied before established")
	}
	directAfter, queuedAfter, _ := resetTcpAckCounters()
	if directAfter != directBefore {
		t.Fatal("pre-SYN ack counted as direct")
	}
	_ = queuedAfter
	_ = queuedBefore
}

// The socket reader waiting on the receive ack condition wakes on a directly
// applied ack (the condition is created lazily by the direct path).
func TestTcpAckDirectWakesBlockedReader(t *testing.T) {
	withTcpAckDirect(t, true)
	h := establishedHarness(t)
	cond := make(chan struct{})
	go func() {
		c := h.seq.receiveAckCondition()
		h.seq.mutex.Lock()
		defer h.seq.mutex.Unlock()
		for h.seq.receiveWindowSize < 500 {
			c.Wait()
		}
		close(cond)
	}()
	time.Sleep(20 * time.Millisecond)
	h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 600})
	select {
	case <-cond:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
}

// The differential harness: the same valid packet script must produce the
// same delivered bytes and the same final ack/window with the direct path on
// and off. (Reordered-shrink scripts are intentionally NOT here: monotonic
// vs overwrite legitimately differ there, and the dedicated tests above pin
// the new behavior.)
func runAckScript(t *testing.T, direct bool, steps []*parsedTcp) semanticState {
	withTcpAckDirect(t, direct)
	h := newPureAckHarness(t)
	delivered := 0
	for stepIndex, tcp := range steps {
		if 0 < len(tcp.payload) {
			delivered += len(tcp.payload)
		}
		h.feed(t, tcp)
		if stepIndex%50 == 49 {
			h.settle(t, delivered)
		}
	}
	h.settle(t, delivered)
	return h.semantic()
}

func ackScriptSteps() []*parsedTcp {
	const isn = pureAckIsn
	// conformant: the advertised edge (ack + window) never shrinks; the
	// zero-window closure happens at the edge the peer already advertised
	return []*parsedTcp{
		{syn: true, seq: isn, windowSize: 100},
		{ack: true, seq: isn + 1, ackNumber: isn + 1, windowSize: 100},
		{ack: true, psh: true, seq: isn + 1, ackNumber: isn + 1, windowSize: 100, payload: []byte("hello")},
		{ack: true, seq: isn + 6, ackNumber: isn + 6, windowSize: 95},                                         // same edge 1101
		{ack: true, seq: isn + 6, ackNumber: isn + 6, windowSize: 95},                                         // duplicate
		{ack: true, seq: isn + 6, ackNumber: isn + 101, windowSize: 0},                                        // closure at the edge
		{ack: true, seq: isn + 6, ackNumber: isn + 101, windowSize: 99},                                       // reopen
		{ack: true, psh: true, seq: isn + 6, ackNumber: isn + 111, windowSize: 89, payload: []byte("world!")}, // edge 1200
		{ack: true, seq: isn + 12, ackNumber: isn + 112, windowSize: 288},                                     // edge 1400
		{ack: true, fin: true, seq: isn + 12, ackNumber: isn + 112, windowSize: 288},
	}
}

func TestTcpAckDirectDifferentialOldVsNew(t *testing.T) {
	oldState := runAckScript(t, false, ackScriptSteps())
	newState := runAckScript(t, true, ackScriptSteps())
	if !reflect.DeepEqual(oldState, newState) {
		t.Fatalf("state diverged\nqueued(old): %+v\ndirect(new): %+v", oldState, newState)
	}
	if oldState.Delivered != "helloworld!" {
		t.Fatalf("delivered bytes wrong: %q", oldState.Delivered)
	}
}

// The seeded interleaving sweep: random valid scripts must land on identical
// final states in both modes, with no stalls. 200 scripts x 500 steps.
func TestTcpAckDirectRandomInterleavings(t *testing.T) {
	rng := rand.New(rand.NewSource(20261016))
	directApplied := int64(0)
	for script := 0; script < 200; script++ {
		// the two directions: peerSeq is the peer's data sequence (packet
		// seq), ackNumber is the peer acking our return side (bounded by
		// receiveSeq, which the harness advanced). Valid traffic only:
		// monotonic acks and windows, so both models must agree.
		const returnBytes = uint32(4096)
		steps := []*parsedTcp{{syn: true, seq: pureAckIsn, windowSize: 100}}
		peerSeq := pureAckIsn + 1
		ackNumber := pureAckIsn + 1
		window := uint16(100)
		edge := ackNumber + uint32(window)
		for step := 0; step < 499; step++ {
			var payload []byte
			switch rng.Intn(10) {
			case 0:
				// a conformant zero-window closure: the ack advances to the
				// edge the peer already advertised (a closure at a stale ack
				// would be a shrink, which the models intentionally disagree
				// on, so it must not appear in valid traffic)
				ackNumber = edge
				window = 0
			case 1:
				window = uint16(100 + rng.Intn(500))
			case 2:
				// duplicate of the previous values
			case 3:
				payload = []byte("x")
			default:
				if ackNumber < pureAckIsn+returnBytes {
					ackNumber += uint32(1 + rng.Intn(5))
				}
			}
			// conformance: a receiver never retracts the right edge it has
			// already advertised, so a window that would shrink the edge is
			// widened up to it (an edge shrink is not valid traffic)
			if window != 0 && ackNumber+uint32(window) < edge {
				window = uint16(edge - ackNumber)
			}
			if e := ackNumber + uint32(window); edge < e {
				edge = e
			}
			steps = append(steps, &parsedTcp{
				ack: true, psh: 0 < len(payload), seq: peerSeq,
				ackNumber: ackNumber, windowSize: window, payload: payload,
			})
			peerSeq += uint32(len(payload))
		}
		steps = append(steps, &parsedTcp{ack: true, fin: true, seq: peerSeq, ackNumber: ackNumber, windowSize: window})
		directBefore, _, _ := resetTcpAckCounters()
		newState := runAckScript(t, true, steps)
		directAfter, _, _ := resetTcpAckCounters()
		directApplied += directAfter - directBefore
		oldState := runAckScript(t, false, steps)
		if !reflect.DeepEqual(oldState, newState) {
			// find the first diverging step for the report
			for stepIndex := range steps {
				o := runAckScript(t, false, steps[:stepIndex+1])
				n := runAckScript(t, true, steps[:stepIndex+1])
				if !reflect.DeepEqual(o, n) {
					t.Fatalf("script %d first diverges at step %d (packet %+v)\nqueued(old): %+v\ndirect(new): %+v", script, stepIndex, steps[stepIndex], o, n)
				}
			}
			t.Fatalf("script %d diverged\nqueued(old): %+v\ndirect(new): %+v", script, oldState, newState)
		}
	}
	if directApplied == 0 {
		t.Fatal("the direct path never applied an ack across the sweep")
	}
}

// Concurrent direct acks and queued data must stay race free and keep the
// ack monotonic.
func TestTcpAckConcurrentStress(t *testing.T) {
	withTcpAckDirect(t, true)
	h := establishedHarness(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ack := pureAckIsn + 1 + uint32(worker)
			for i := 0; i < 200; i++ {
				ok, err := h.seq.sendOrApplyPureAck(
					protocol.ProvideMode_Network,
					&parsedTcp{ack: true, seq: ack, ackNumber: ack, windowSize: 100},
					MessagePoolGet(64), -1)
				if !ok || err != nil {
					t.Errorf("feed: ok=%v err=%v", ok, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	h.settle(t, 0)
	s := h.state()
	if 0 > int32(s.ReceiveSeqAck-pureAckIsn-1) {
		t.Fatalf("ack moved backwards under stress: %+v", s)
	}
}

func benchTcpAck(b *testing.B, direct bool) {
	previous := tcpAckDirectOverride.Load()
	if direct {
		tcpAckDirectOverride.Store(tcpAckDirectOn)
	} else {
		tcpAckDirectOverride.Store(tcpAckDirectOff)
	}
	defer tcpAckDirectOverride.Store(previous)
	h := newPureAckHarness(b)
	h.feed(b, &parsedTcp{syn: true, seq: pureAckIsn, windowSize: 100})
	h.waitEstablished(b)
	tcp := &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.seq.sendOrApplyPureAck(protocol.ProvideMode_Network, tcp, MessagePoolGet(64), -1)
	}
}

func BenchmarkTcpAckQueued(b *testing.B) { benchTcpAck(b, false) }
func BenchmarkTcpAckDirect(b *testing.B) { benchTcpAck(b, true) }

// A: the application is commutative and idempotent: every permutation of a
// set of acks, fed twice (duplicates), lands on the same fixed state.
func TestTcpAckApplicationIsCommutativeAndIdempotent(t *testing.T) {
	withTcpAckDirect(t, true)
	base := []*parsedTcp{
		{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 10, windowSize: 100},
		{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 50, windowSize: 300},
		{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 30, windowSize: 900},
		{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 50, windowSize: 50},
		{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 20, windowSize: 200},
	}
	// the fixed point: ack = max ack; edge = max(ack+window) = 30+900
	const wantAck, wantEnd = pureAckIsn + 50, pureAckIsn + 930
	wantWindow := uint32(wantEnd - wantAck)
	orders := [][]int{
		{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 4, 0, 3, 1}, {3, 1, 4, 0, 2}, {1, 2, 3, 4, 0},
	}
	for orderIndex, order := range orders {
		h := establishedHarness(t)
		for _, i := range order {
			h.feed(t, base[i])
		}
		for _, i := range order { // duplicates: idempotent
			h.feed(t, base[i])
		}
		s := h.settle(t, 0)
		if s.ReceiveSeqAck != wantAck || s.WindowEnd != wantEnd || s.ReceiveWindow != wantWindow {
			t.Fatalf("order %d (%v): %+v, want ack %d end %d window %d", orderIndex, order, s, wantAck, wantEnd, wantWindow)
		}
	}
}

// B: each unusual packet class fails closed into the ordered queue and never
// touches the direct path.
func TestTcpAckDirectFallsBackToTheQueue(t *testing.T) {
	withTcpAckDirect(t, true)
	cases := []struct {
		name  string
		setup func(t *testing.T, h *pureAckHarness)
		tcp   *parsedTcp
	}{
		{"payload present", nil, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100, payload: []byte("x")}},
		{"syn flag", nil, &parsedTcp{syn: true, ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100}},
		{"fin flag", nil, &parsedTcp{ack: true, fin: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100}},
		{"rst flag", nil, &parsedTcp{ack: true, rst: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100}},
		{"ack beyond sent", nil, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 100000, windowSize: 100}},
		{"ack behind acked", func(t *testing.T, h *pureAckHarness) {
			h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 40, windowSize: 100})
			h.settle(t, 0)
		}, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 10, windowSize: 100}},
		{"edge beyond maximum", func(t *testing.T, h *pureAckHarness) {
			// the harness SYN negotiates no window scale; force one
			h.seq.mutex.Lock()
			h.seq.receiveWindowScale = 10
			h.seq.mutex.Unlock()
		}, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 0xFFFF}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := establishedHarness(t)
			if c.setup != nil {
				c.setup(t, h)
			}
			resetTcpAckCounters()
			h.feed(t, c.tcp)
			h.settle(t, len(c.tcp.payload))
			if tcpAckDirectAppliedCount.Load() != 0 {
				t.Fatalf("%s: applied directly", c.name)
			}
			pure := !c.tcp.syn && !c.tcp.fin && !c.tcp.rst && len(c.tcp.payload) == 0
			if pure && tcpAckQueuedCount.Load() == 0 {
				t.Fatalf("%s: a pure ack must count as queued", c.name)
			}
			if !pure && tcpAckQueuedCount.Load() != 0 {
				t.Fatalf("%s: a non-pure packet must not count as a queued pure ack", c.name)
			}
		})
	}
	// context done: refused directly via the raw entry (the queue itself is
	// dead once the context ends, so the feed path is not exercised)
	h := establishedHarness(t)
	h.cancel()
	if h.seq.applyEstablishedPureAck(&parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100}) {
		t.Fatal("applied directly with a dead context")
	}
}

// D: the trip mechanism flips the process to the old path, once.
func TestTcpAckTripMechanism(t *testing.T) {
	tcpAckTripped.Store(0)
	withTcpAckDirect(t, true)
	h := establishedHarness(t)
	tripTcpAckDirect(h.seq.log, "test")
	if tcpAckTripped.Load() != 1 {
		t.Fatal("trip not recorded")
	}
	if tcpAckDirectEnabled() {
		t.Fatal("trip did not flip the switch")
	}
	// a second trip is a no-op
	countBefore := tcpAckInvariantViolations.Load()
	tripTcpAckDirect(h.seq.log, "again")
	if tcpAckInvariantViolations.Load() != countBefore {
		t.Fatal("second trip counted")
	}
	t.Cleanup(func() { tcpAckTripped.Store(0) })
}

// E: every path returns the packet exactly once (ActiveBuffers back to the
// baseline, never below).
func TestTcpAckPoolOwnershipExactOnce(t *testing.T) {
	withTcpAckDirect(t, true)
	baseline := globalPoolMetrics.ActiveBuffers.Load()
	for i := 0; i < 200; i++ {
		h := establishedHarness(t)
		// direct
		h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100})
		// queued payload (ownership moves into the write path)
		h.feed(t, &parsedTcp{ack: true, psh: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100, payload: []byte("data")})
		// queued stale duplicate
		h.feed(t, &parsedTcp{ack: true, seq: pureAckIsn + 1, ackNumber: pureAckIsn + 1, windowSize: 100})
		h.settle(t, 4)
	}
	active := globalPoolMetrics.ActiveBuffers.Load()
	if active > baseline {
		t.Fatalf("pool leak: %d buffers outstanding over baseline", active-baseline)
	}
	if active < baseline {
		t.Fatalf("double return: %d buffers under baseline", baseline-active)
	}
}
