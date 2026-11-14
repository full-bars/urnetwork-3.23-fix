package connect

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newTestRetryBudget() (*transferRetryBudget, *time.Time) {
	now := time.Unix(3_000_000, 0)
	b := newTransferRetryBudget(func() time.Time { return now })
	return b, &now
}

func TestRetryBudget_StartsWithAGrantSoAFreshProcessCanRecover(t *testing.T) {
	b, _ := newTestRetryBudget()
	if !b.allow(1000) {
		t.Fatal("a fresh budget must allow a resend")
	}
}

func TestRetryBudget_ResendsAreChargedAgainstWhatWasFirstSent(t *testing.T) {
	b, _ := newTestRetryBudget()
	b.reset(0)

	if b.allow(1) {
		t.Fatal("an empty budget must refuse")
	}
	b.earn(10_000) // 10 KB first-sent earns 25%
	if !b.allow(2500) {
		t.Fatal("2.5 KB of resends is exactly the 25% earned")
	}
	if b.allow(1) {
		t.Fatal("that spent it all")
	}
}

func TestRetryBudget_FloorRefillsOverTimeSoQuietFlowsStillRecover(t *testing.T) {
	b, now := newTestRetryBudget()
	b.reset(0)
	*now = now.Add(time.Second)
	if !b.allow(retryBudgetFloorBytesPerSecond) {
		t.Fatal("one second must refill the floor rate")
	}
	if b.allow(1000) {
		t.Fatal("but not more than the floor rate")
	}
}

func TestRetryBudget_BurstIsCapped(t *testing.T) {
	b, now := newTestRetryBudget()
	b.reset(0)
	b.earn(1 << 30)
	*now = now.Add(24 * time.Hour)
	if !b.allow(retryBudgetBurstBytes) {
		t.Fatal("a full bucket must pay out its burst")
	}
	if b.allow(1) {
		t.Fatal("the bucket never holds more than the burst, however long it sits")
	}
}

func TestRetryBudget_RefusalsAreNotCharged(t *testing.T) {
	b, _ := newTestRetryBudget()
	b.reset(100)
	if b.allow(1000) {
		t.Fatal("too big")
	}
	if !b.allow(100) {
		t.Fatal("a refused resend must leave the tokens alone")
	}
}

// The incident: a peer that never acks. Every unacked frame used to be resent up
// to 16 times, so a node sent many times what it delivered. With the budget the
// resends are bounded by what was first sent plus a small floor.
func TestSendSequence_ResendsToANonAckingPeerAreBudgeted(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 2 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 4 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Minute
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	// a route that swallows everything and never acks: the slow far end
	sink := make(chan []byte, 1<<16)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sink})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink:
			}
		}
	}()
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, clientSettings.SendBufferSettings)

	const items = 200
	const frameBytes = 1000
	for i := 0; i < items; i++ {
		item := &sendItem{
			transferItem: transferItem{
				messageId:        NewId(),
				sequenceNumber:   uint64(i + 1),
				messageByteCount: frameBytes,
			},
			sendTime:           time.Now(),
			resendTime:         time.Now().Add(-time.Second),
			sendCount:          3, // the third send onward is charged; earlier ones are free
			head:               true,
			transferFrameBytes: make([]byte, frameBytes),
			ackCallback:        func(err error) {},
		}
		seq.sendItems = append(seq.sendItems, item)
		seq.resendQueue.Add(item)
	}

	// 20 KB to spend, nothing first-sent: only the grant and the floor may pay
	transferResendBudget.reset(20_000)
	before := TransferSendStatsNow()

	done := make(chan struct{})
	go HandleError(func() { defer close(done); seq.Run() })
	// stop the resend loop before this test ends: the tests that follow swap the
	// shared budget's clock in and out, and a goroutine still inside
	// retryAllowed would race those writes
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(600 * time.Millisecond)

	after := TransferSendStatsNow()
	resent := after.ResendBytes - before.ResendBytes
	// 20 KB grant + 64 KiB/s floor for 0.6 s (about 39 KB), with slack for the loop
	floorRate := float64(retryBudgetFloorBytesPerSecond)
	limit := int64(20_000 + 0.6*floorRate + 3*frameBytes)
	if resent > limit {
		t.Fatalf("resent %d bytes, the budget allows about %d (unbudgeted this would be ~%d)", resent, limit, items*15*frameBytes)
	}
	if resent == 0 {
		t.Fatal("the budget must still let some resends through")
	}
	if after.ResendDeferredFrames-before.ResendDeferredFrames == 0 {
		t.Fatal("refused resends must be counted as deferred")
	}

	// earning from first sends releases more
	transferResendBudget.earn(400_000) // +100 KB
	time.Sleep(400 * time.Millisecond)
	if got := TransferSendStatsNow().ResendBytes - after.ResendBytes; got < 40_000 {
		t.Fatalf("earned budget should have released more resends, only %d bytes went", got)
	}
}

// shadow and off never delay a resend
func TestSendSequence_ResendBudgetIsOnlyEnforcedInEnforceMode(t *testing.T) {
	defer setGovernModeForTest(governShadow)()
	transferResendBudget.reset(0)
	if allowed, _ := retryAllowed(1000); !allowed {
		t.Fatal("shadow mode must never refuse a resend")
	}
	defer setGovernModeForTest(governOff)()
	if allowed, _ := retryAllowed(1000); !allowed {
		t.Fatal("off mode must never refuse a resend")
	}
	restore := setGovernModeForTest(governEnforce)
	defer restore()
	if allowed, _ := retryAllowed(1000); allowed {
		t.Fatal("enforce mode refuses when the budget is empty")
	}
}

// A resend the budget refuses is not a timeout and must not be treated as one:
// before this was fixed the gate ran after the unreliable-flight bookkeeping, so
// every deferral halved the flight window and forgot the flight for a send that
// never happened.
func TestSendSequence_DeferredResendHasNoFlightSideEffects(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 2 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 4 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Minute
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	sink := make(chan []byte, 1<<16)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sink})
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, clientSettings.SendBufferSettings)
	seq.flightController = newSendFlightController(clientSettings.SendBufferSettings)

	items := make([]*sendItem, 0, 50)
	for i := 0; i < 50; i++ {
		item := &sendItem{
			transferItem:            transferItem{messageId: NewId(), sequenceNumber: uint64(i + 1), messageByteCount: 200_000},
			sendTime:                time.Now(),
			resendTime:              time.Now().Add(-time.Second),
			sendCount:               5, // past the head's free window too, so every resend is charged
			head:                    true,
			transferFrameBytes:      make([]byte, 200_000),
			ackCallback:             func(err error) {},
			unreliableFlightTracked: true,
		}
		items = append(items, item)
		seq.sendItems = append(seq.sendItems, item)
		seq.resendQueue.Add(item)
	}

	// the floor refills 64 KiB/s, less than one of these 200 KB frames in the test's
	// 300 ms, so nothing can be paid for: every charged resend is deferred
	transferResendBudget.reset(0)
	timeoutsBefore := client.unreliableFlightTimeoutCount.Load()
	deferredBefore := TransferSendStatsNow().ResendDeferredFrames

	done := make(chan struct{})
	go func() { defer close(done); seq.Run() }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	if TransferSendStatsNow().ResendDeferredFrames == deferredBefore {
		t.Fatal("setup: expected resends to be deferred")
	}
	if got := client.unreliableFlightTimeoutCount.Load() - timeoutsBefore; got != 0 {
		t.Fatalf("a deferred resend recorded %d unreliable-flight timeouts for sends that never happened", got)
	}
	for i, item := range items {
		if !item.unreliableFlightTracked {
			t.Fatalf("item %d lost its unreliable flight because a resend was deferred", i)
		}
	}
}

// A frame that was lost once must always be recoverable, whatever the budget
// says: the first resend is free, and the frame at the head of the sequence, the
// one every later frame waits behind, gets a head start of further free resends.
func TestSendSequence_FirstResendIsFreeAndTheHeadGetsAHeadStart(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 2 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 4 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Minute
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	sink := make(chan []byte, 1<<16)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sink})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink:
			}
		}
	}()
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, clientSettings.SendBufferSettings)

	mk := func(n uint64) *sendItem {
		return &sendItem{
			transferItem:       transferItem{messageId: NewId(), sequenceNumber: n, messageByteCount: 200_000},
			sendTime:           time.Now(),
			resendTime:         time.Now().Add(-time.Second),
			sendCount:          1,
			head:               true,
			transferFrameBytes: make([]byte, 200_000),
			ackCallback:        func(err error) {},
		}
	}
	head, other := mk(1), mk(2)
	for _, item := range []*sendItem{head, other} {
		seq.sendItems = append(seq.sendItems, item)
		seq.resendQueue.Add(item)
	}

	// an exhausted budget; the frames are bigger than the floor can refill in 300 ms
	transferResendBudget.reset(0)
	done := make(chan struct{})
	go func() { defer close(done); seq.Run() }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	if other.sendCount != 2 {
		t.Fatalf("a non-head frame gets one free resend and then waits for the budget: sendCount=%d, want 2", other.sendCount)
	}
	if head.sendCount != 5 {
		t.Fatalf("the head frame gets free resends through the 5th send: sendCount=%d, want 5", head.sendCount)
	}
}

// Budget comes from bytes the far end acknowledged. A first send earns nothing,
// and an acknowledged frame earns a quarter of its size.
func TestRetryBudget_EarnsOnAcknowledgedBytesNotSentBytes(t *testing.T) {
	transferResendBudget.reset(0)
	frozen := time.Now()
	previousNow := transferResendBudget.now
	transferResendBudget.now = func() time.Time { return frozen }
	defer func() { transferResendBudget.now = previousNow }()
	transferResendBudget.reset(0)

	before := TransferSendStatsNow()
	recordTransferFirstSend(10_000)
	if got := TransferRetryBudgetTokens(); got != 0 {
		t.Fatalf("a first send earned %.0f tokens, want 0", got)
	}
	recordTransferAckedBytes(10_000)
	if got := TransferRetryBudgetTokens(); got != retryBudgetRatio*10_000 {
		t.Fatalf("an acknowledged frame earned %.0f tokens, want %.0f", got, retryBudgetRatio*10_000)
	}
	if got := TransferSendStatsNow().BudgetEarnedBytes - before.BudgetEarnedBytes; got != 10_000 {
		t.Fatalf("earned bytes counter moved %d, want 10000", got)
	}
}

// Shadow sends anyway but reports what enforce would have refused.
func TestRetryBudget_ShadowSendsAndCountsWhatEnforceWouldRefuse(t *testing.T) {
	frozen := time.Now()
	previousNow := transferResendBudget.now
	transferResendBudget.now = func() time.Time { return frozen }
	defer func() { transferResendBudget.now = previousNow }()
	transferResendBudget.reset(0)
	defer setGovernModeForTest(governShadow)()

	before := TransferSendStatsNow()
	if allowed, _ := retryAllowed(1000); !allowed {
		t.Fatal("shadow must send")
	}
	after := TransferSendStatsNow()
	if after.ResendWouldDeferFrames-before.ResendWouldDeferFrames != 1 || after.ResendWouldDeferBytes-before.ResendWouldDeferBytes != 1000 {
		t.Fatalf("shadow did not count the would-be refusal: %+v -> %+v", before, after)
	}
	if after.ResendDeferredFrames != before.ResendDeferredFrames {
		t.Fatal("shadow must not count a real deferral")
	}

	restore := setGovernModeForTest(governOff)
	defer restore()
	if allowed, _ := retryAllowed(1000); !allowed || TransferSendStatsNow().ResendWouldDeferFrames != after.ResendWouldDeferFrames {
		t.Fatal("off must neither refuse nor report")
	}
}

// Refusals become one log line per interval, naming the mode.
func TestRetryBudget_ReportsRefusalsOncePerInterval(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	var lines []string
	reporter := &retryBudgetReporter{
		now:  func() time.Time { return clock },
		emit: func(line string) { lines = append(lines, line) },
	}
	reporter.note(true, 1000) // starts the interval
	reporter.note(true, 2000)
	clock = clock.Add(retryBudgetReportInterval - time.Second)
	reporter.note(true, 3000)
	if len(lines) != 0 {
		t.Fatalf("logged inside the interval: %v", lines)
	}
	clock = clock.Add(2 * time.Second)
	reporter.note(false, 4000)
	if len(lines) != 1 {
		t.Fatalf("want one line after the interval, got %v", lines)
	}
	if !strings.Contains(lines[0], "[govern] retry budget") || !strings.Contains(lines[0], "shadow") || !strings.Contains(lines[0], "4 resends") {
		t.Fatalf("unexpected line: %s", lines[0])
	}
}

// retryAllowed reports what it debited so a failed write can give it back, and
// the refund never overshoots the burst cap.
func TestRetryBudget_RefundReturnsExactlyWhatWasDebited(t *testing.T) {
	frozen := time.Now()
	previousNow := transferResendBudget.now
	transferResendBudget.now = func() time.Time { return frozen }
	defer func() { transferResendBudget.now = previousNow }()
	transferResendBudget.reset(5_000)

	allowed, debited := retryAllowed(1_200)
	if !allowed || debited != 1_200 {
		t.Fatalf("allowed=%t debited=%d, want the full 1200 charged", allowed, debited)
	}
	if got := TransferRetryBudgetTokens(); got != 3_800 {
		t.Fatalf("tokens after the debit = %.0f, want 3800", got)
	}
	refundRetry(debited)
	if got := TransferRetryBudgetTokens(); got != 5_000 {
		t.Fatalf("tokens after the refund = %.0f, want 5000", got)
	}
	refundRetry(0) // a free or uncharged resend refunds nothing
	if got := TransferRetryBudgetTokens(); got != 5_000 {
		t.Fatalf("a zero refund changed the balance to %.0f", got)
	}
	transferResendBudget.reset(retryBudgetBurstBytes)
	refundRetry(1 << 20)
	if got := TransferRetryBudgetTokens(); got != float64(retryBudgetBurstBytes) {
		t.Fatalf("a refund overshot the burst cap: %.0f", got)
	}

	// off mode and a shadow resend the budget cannot pay for debit nothing
	restore := setGovernModeForTest(governOff)
	if allowed, debited := retryAllowed(1_000); !allowed || debited != 0 {
		t.Fatalf("off: allowed=%t debited=%d", allowed, debited)
	}
	restore()
	transferResendBudget.reset(0)
	restore = setGovernModeForTest(governShadow)
	defer restore()
	if allowed, debited := retryAllowed(1_000); !allowed || debited != 0 {
		t.Fatalf("shadow with an empty budget: allowed=%t debited=%d, want sent but nothing debited", allowed, debited)
	}
}

// A resend whose write fails must not use up budget: with no carrier at all every
// resend write fails, and the budget has to come back whole.
func TestSendSequence_FailedResendWriteRefundsTheBudget(t *testing.T) {
	frozen := time.Now()
	previousNow := transferResendBudget.now
	transferResendBudget.now = func() time.Time { return frozen }
	defer func() { transferResendBudget.now = previousNow }()
	transferResendBudget.reset(50_000)

	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 2 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 4 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Minute
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour
	clientSettings.SendBufferSettings.WriteTimeout = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings) // no transport: writes fail
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, clientSettings.SendBufferSettings)
	for i := 0; i < 5; i++ {
		item := &sendItem{
			transferItem:       transferItem{messageId: NewId(), sequenceNumber: uint64(i + 1), messageByteCount: 1000},
			sendTime:           time.Now(),
			resendTime:         time.Now().Add(-time.Second),
			sendCount:          9, // deep enough that the resend is charged
			head:               true,
			transferFrameBytes: make([]byte, 1000),
			ackCallback:        func(error) {},
		}
		seq.sendItems = append(seq.sendItems, item)
		seq.resendQueue.Add(item)
	}
	before := TransferSendStatsNow()
	done := make(chan struct{})
	go HandleError(func() { defer close(done); seq.Run() })
	time.Sleep(400 * time.Millisecond)
	// stop the loop and let any write still in flight fail and refund
	cancel()
	time.Sleep(150 * time.Millisecond)
	// the loop has stopped: it must not read the budget while this test's
	// deferred cleanup restores the clock it swapped
	<-done

	attempts := TransferSendStatsNow().ResendFrames - before.ResendFrames
	if attempts == 0 {
		t.Fatal("no resend was attempted, so the test measures nothing")
	}
	if got := TransferRetryBudgetTokens(); got != 50_000 {
		t.Fatalf("budget = %.0f after %d resend attempts whose writes all failed, want the full 50000 back", got, attempts)
	}
}
