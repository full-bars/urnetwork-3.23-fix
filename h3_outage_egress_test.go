package connect

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// silentQuicLadderBytes is what one QUIC dial costs when the peer never answers:
// the Initial and its retransmits, each padded to the 1200 byte minimum.
const silentQuicLadderBytes = 16 * 1200

// handshakeFor starts a traced handshake whose packets are charged to stats, the
// way dialH3 wires it, and returns the recorder events are fed to.
func handshakeFor(stats *attemptStats) (*h3QuicHandshakeAttempt, qlogwriter.Recorder) {
	packetStats := &H3QuicPacketStats{}
	attempt := packetStats.beginHandshakeAttempt()
	attempt.connect = stats
	recorder := packetStats.tracerForAttempt(attempt)(context.Background(), true, quic.ConnectionID{}).AddProducer()
	return attempt, recorder
}

func TestH3HandshakeBytesAreChargedAsEachPacketIsTraced(t *testing.T) {
	stats := &attemptStats{}
	attempt, recorder := handshakeFor(stats)
	for i := 0; i < 16; i++ {
		recorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
		// the dial has not returned yet, and the bytes are already on the attempt: a
		// dial canceled mid-ladder returns after the governor has read the attempt
		if got := stats.tx.Load(); got != int64(i+1)*1200 {
			t.Fatalf("after %d packets tx=%d, want %d", i+1, got, (i+1)*1200)
		}
	}
	if got := stats.tx.Load(); got != silentQuicLadderBytes {
		t.Fatalf("tx=%d, want %d", got, silentQuicLadderBytes)
	}
	if got := stats.rx.Load(); got != 0 {
		t.Fatalf("a silent peer answered nothing but rx=%d", got)
	}
	recorder.RecordEvent(qlog.PacketReceived{Raw: qlog.RawInfo{Length: 1500}})
	if got := stats.rx.Load(); got != 1500 {
		t.Fatalf("rx=%d, want 1500", got)
	}
	attempt.finish(true)
}

// The session that follows a connected handshake is not part of the attempt.
func TestH3HandshakeBytesStopBeingChargedOnceTheHandshakeIsOver(t *testing.T) {
	stats := &attemptStats{}
	attempt, recorder := handshakeFor(stats)
	recorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	attempt.finish(true)
	recorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	recorder.RecordEvent(qlog.PacketReceived{Raw: qlog.RawInfo{Length: 1500}})
	if tx, rx := stats.tx.Load(), stats.rx.Load(); tx != 1200 || rx != 0 {
		t.Fatalf("tx=%d rx=%d, want only the handshake's 1200 sent", tx, rx)
	}
}

func TestH3HandshakeWithoutAConnectAttemptChargesNothingAndDoesNotPanic(t *testing.T) {
	_, recorder := handshakeFor(nil)
	recorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	var none *h3QuicHandshakeAttempt
	none.chargeConnect(10, true)
}

// A silent edge answers nothing, so the verdict stays unsure. The bytes the QUIC
// ladders burn must still cut the rate once the attempts carry them.
func TestBudget_SilentQuicLaddersCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	windowBytes := 2 * budgetFailedBytesCeilingPerSec * int(budgetRecentWindow.Seconds())
	n := windowBytes/silentQuicLadderBytes + budgetMinByteBreakerFailures
	h.failedBytes(connectTimeoutNoReply, n, silentQuicLadderBytes)
	h.budget.control()
	if got := h.budget.currentRate(); got >= budgetCeilingRate {
		t.Fatalf("%d silent QUIC ladders burned %d bytes and the rate is still %v", n, n*silentQuicLadderBytes, got)
	}
}

// Canceled attempts are not outcomes, but their packets are on the wire. Two of
// every three QUIC dials against a silent edge end this way.
func TestBudget_CanceledQuicBytesCountTowardTheBreaker(t *testing.T) {
	h := newBudgetHarness()
	failures := budgetMinByteBreakerFailures + 5
	// the failed attempts alone stay under the ceiling
	h.failedBytes(connectTimeoutNoReply, failures, 1_000)
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("setup: cheap failures must not cut")
	}

	h = newBudgetHarness()
	h.failedBytes(connectTimeoutNoReply, failures, 1_000)
	canceled := 2 * budgetFailedBytesCeilingPerSec * int(budgetRecentWindow.Seconds()) / silentQuicLadderBytes
	for i := 0; i < canceled; i++ {
		st := &attemptStats{}
		st.tx.Store(silentQuicLadderBytes)
		h.gov.recordCanceledBytes(st)
	}
	h.budget.control()
	if h.budget.currentRate() >= budgetCeilingRate {
		t.Fatal("canceled QUIC attempts burned the bytes and the breaker did not cut")
	}
}

func TestBudget_CanceledBytesAloneAreNotAnOutage(t *testing.T) {
	h := newBudgetHarness()
	for i := 0; i < 2000; i++ {
		st := &attemptStats{}
		st.tx.Store(silentQuicLadderBytes)
		h.gov.recordCanceledBytes(st)
	}
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("teardown of attempts that never failed must not throttle: the breaker needs most attempts failing")
	}
	if got := h.gov.snapshot().Attempts; got != 0 {
		t.Fatalf("a canceled attempt is not an attempt, got %d", got)
	}
}

// withEnforcedBudget swaps in a private governor and budget in enforce mode.
func withEnforcedBudget(t *testing.T) *reconnectBudget {
	t.Helper()
	t.Cleanup(setGovernModeForTest(governEnforce))
	previousGov, previousBudget := connectGov, connectBudget
	t.Cleanup(func() { connectGov, connectBudget = previousGov, previousBudget })
	connectGov = newConnectGovernor(time.Now)
	connectBudget = newReconnectBudget(connectGov, time.Now)
	connectBudget.emit = func(string) {}
	return connectBudget
}

func h3TransportForProxy(proxyIndex int, proxied bool) *PlatformTransport {
	settings := &ClientStrategySettings{}
	if proxied {
		settings.ProxySettings = &ProxySettings{Index: proxyIndex}
	}
	return &PlatformTransport{clientStrategy: &ClientStrategy{settings: settings}}
}

func admitWithin(transport *PlatformTransport, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return transport.admitH3Dial(ctx)
}

func TestAdmitH3Dial_ProxiedQuicWaitsWhileTheBudgetIsThrottled(t *testing.T) {
	budget := withEnforcedBudget(t)
	proxied := h3TransportForProxy(7, true)
	if !admitWithin(proxied, time.Second) {
		t.Fatal("a healthy budget admits at once")
	}
	budget.setRate(budgetFloorRate)
	if admitWithin(proxied, 100*time.Millisecond) {
		t.Fatal("a proxied QUIC dial ran straight through a throttled budget")
	}
}

func TestAdmitH3Dial_DirectAndPinnedQuicNeverWait(t *testing.T) {
	budget := withEnforcedBudget(t)
	budget.setRate(budgetFloorRate)
	if !admitWithin(h3TransportForProxy(0, false), 100*time.Millisecond) {
		t.Fatal("the direct identity must never wait on the budget")
	}
	if !admitWithin(h3TransportForProxy(0, true), 100*time.Millisecond) {
		t.Fatal("proxy index 0 is the direct path and must never wait")
	}
	pinned := h3TransportForProxy(7, true)
	pinned.ipFamily = 4
	if !admitWithin(pinned, 100*time.Millisecond) {
		t.Fatal("a family-pinned transport paces itself and must not wait on the budget")
	}
}

// The proxy's `parked` flag is shared with its H1 loop and read by
// connectingActive, which keeps the reaper away from a proxy waiting its turn. An
// H3 waiter that set and then cleared it would clear the H1's mark early.
func TestAdmitH3Dial_NeverMarksTheProxyParked(t *testing.T) {
	resetProxyHealthForTest()
	budget := withEnforcedBudget(t)
	RegisterProxy(7, "192.0.2.7:1080", "192.0.2.7:1080")
	markProxyParked(7, true) // its H1 loop is waiting
	budget.setRate(budgetFloorRate)
	if admitWithin(h3TransportForProxy(7, true), 100*time.Millisecond) {
		t.Fatal("setup: the budget is throttled, the H3 dial must wait")
	}
	proxyHealthMu.Lock()
	parked := proxyHealthByIndex[7].parked
	proxyHealthMu.Unlock()
	if !parked {
		t.Fatal("an H3 waiter cleared the mark its proxy's H1 loop set")
	}
}

// H1 is how the platform is probed, so it is served before an auxiliary carrier.
// admitConnectQuiet queues with no score, which ranks behind every scored waiter.
func TestAdmitConnectQuiet_RanksBehindAScoredH1Waiter(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	h.budget.pick = func(n int) int { return 0 } // the random slot would hide the order

	quiet := make(chan bool, 1)
	go func() { quiet <- h.budget.admitParked(context.Background(), false, nil, nil) }()
	waitQueued(t, h.budget, 1)
	scored := waiterScored(context.Background(), h.budget, 1<<20)
	waitQueued(t, h.budget, 2)

	h.step(500 * time.Millisecond) // one token
	time.Sleep(50 * time.Millisecond)

	select {
	case <-scored:
	default:
		t.Fatal("the scored H1 waiter was not the one released first")
	}
	select {
	case <-quiet:
		t.Fatal("the quiet H3 waiter was released ahead of, or with, the scored one")
	default:
	}
}

// The H1 loop classifies a failed connect with its attempt's stats, so a timeout
// after the platform replied counts as the platform failing. The H3 loop passes
// the stats for their bytes only and classifies without them, so a QUIC timeout
// stays ambiguous. Swapping the two keeps every count the same and is silent, so
// each call is pinned to the loop it sits in, by the lines just above it.
func TestGovernorFeedSites_H1ClassifiesWithItsStatsAndH3WithoutThem(t *testing.T) {
	src, err := os.ReadFile("transport.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	const h1 = "connectGov.recordAttempt(classifyConnectError(err, attemptStats), attemptStats)"
	const h3 = "connectGov.recordAttempt(classifyConnectError(err, nil), attemptStats)"
	const oldH3 = "connectGov.recordAttempt(classifyConnectError(err, nil), nil)"
	if strings.Count(text, h1) != 1 || strings.Count(text, h3) != 1 || strings.Contains(text, oldH3) {
		t.Fatalf("want exactly one H1 feed and one H3 feed that carries bytes, and no byte-less H3 feed")
	}
	above := func(call string) string {
		i := strings.Index(text, call)
		return text[max(0, i-700):i]
	}
	if !strings.Contains(above(h1), "familyNoteConnectFailed(TransportModeH1,") {
		t.Fatal("the feed that classifies with its stats is not the H1 loop's")
	}
	if !strings.Contains(above(h3), "connectGov.recordCanceledBytes") && !strings.Contains(above(h3), "h3GatedFor(ptMode)") {
		t.Fatal("the feed that classifies without its stats is not the H3 loop's")
	}
}
