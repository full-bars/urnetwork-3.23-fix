package connect

import (
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
)

// silentQuicLadderBytes is what one QUIC dial costs when the peer never answers:
// the Initial and its retransmits, each padded to the 1200 byte minimum.
const silentQuicLadderBytes = 16 * 1200

func recordHandshakePackets(t *testing.T, sent int, received int) *h3QuicHandshakeAttempt {
	t.Helper()
	stats := &H3QuicPacketStats{}
	attempt := stats.beginHandshakeAttempt()
	recorder := stats.tracerForAttempt(attempt)(context.Background(), true, quic.ConnectionID{}).AddProducer()
	for i := 0; i < sent; i++ {
		recorder.RecordEvent(qlog.PacketSent{Raw: qlog.RawInfo{Length: 1200}})
	}
	for i := 0; i < received; i++ {
		recorder.RecordEvent(qlog.PacketReceived{Raw: qlog.RawInfo{Length: 1500}})
	}
	return attempt
}

func TestH3HandshakeWireBytesAreChargedToTheConnectAttemptOnce(t *testing.T) {
	attempt := recordHandshakePackets(t, 16, 0)
	stats := &attemptStats{}
	attempt.creditTo(stats)
	attempt.creditTo(stats) // the dial's error path and its late-failure path may both run
	if got := stats.tx.Load(); got != silentQuicLadderBytes {
		t.Fatalf("tx=%d, want %d", got, silentQuicLadderBytes)
	}
	if got := stats.rx.Load(); got != 0 {
		t.Fatalf("a silent peer answered nothing but rx=%d", got)
	}
}

func TestH3HandshakeReplyBytesLetTheGovernorSeeThePlatformAnswered(t *testing.T) {
	attempt := recordHandshakePackets(t, 4, 1)
	stats := &attemptStats{}
	attempt.creditTo(stats)
	err := context.DeadlineExceeded
	if got := classifyConnectError(err, stats); got != connectTimeoutPlatform {
		t.Fatalf("a timeout after the platform sent %d bytes is %v, want timeout_platform", stats.rx.Load(), got)
	}
	if got := classifyConnectError(err, &attemptStats{}); got != connectTimeoutNoReply {
		t.Fatalf("a timeout in silence is %v, want timeout_silent", got)
	}
}

func TestH3HandshakeCreditToleratesNoStats(t *testing.T) {
	recordHandshakePackets(t, 3, 0).creditTo(nil)
	var none *h3QuicHandshakeAttempt
	none.creditTo(&attemptStats{})
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
