package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type budgetHarness struct {
	gov     *connectGovernor
	now     *time.Time
	budget  *reconnectBudget
	clockMu sync.Mutex
}

func newBudgetHarness() *budgetHarness {
	start := time.Unix(2_000_000, 0)
	now := start
	h := &budgetHarness{}
	// waiter goroutines read the clock while the test advances it
	clock := func() time.Time {
		h.clockMu.Lock()
		defer h.clockMu.Unlock()
		return now
	}
	gov := newConnectGovernor(clock)
	h.gov, h.now, h.budget = gov, &now, newReconnectBudget(gov, clock)
	// tests judge the budget, not the startup grace: start the governor long ago
	gov.started = start.Add(-time.Hour)
	// the real probe reads this machine's memory; tests must not depend on it
	h.budget.memoryPressure = func() bool { return false }
	// nor on how many proxies other tests registered: the attempt cap is off
	h.budget.attemptCap.proxyCount = func() int { return 0 }
	return h
}

func (h *budgetHarness) advance(d time.Duration) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	*h.now = h.now.Add(d)
}

// step advances the fake clock in the dispatcher's own tick size
func (h *budgetHarness) step(total time.Duration) {
	for elapsed := time.Duration(0); elapsed < total; elapsed += budgetTick {
		h.advance(budgetTick)
		h.budget.tick(*h.now)
	}
}

func (h *budgetHarness) attempts(class connectClass, n int) {
	for i := 0; i < n; i++ {
		h.gov.recordAttempt(class, nil)
	}
}

// waiter starts an admit in the background and reports when it is released
func waiter(ctx context.Context, b *reconnectBudget, direct bool) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- b.admit(ctx, direct) }()
	return out
}

func waitQueued(t *testing.T, b *reconnectBudget, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.queued() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters queued", b.queued(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBudget_HealthyPathNeverQueues(t *testing.T) {
	h := newBudgetHarness()
	for i := 0; i < 1000; i++ {
		if !h.budget.admit(context.Background(), false) {
			t.Fatal("an open budget must admit immediately")
		}
	}
	if h.budget.queued() != 0 || h.budget.throttled() {
		t.Fatal("a healthy budget must hold nothing")
	}
}

func TestBudget_DirectPathIsExempt(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	if !h.budget.admit(context.Background(), true) {
		t.Fatal("the direct path must never wait on the budget")
	}
	if h.budget.queued() != 0 {
		t.Fatal("the direct path must not occupy the queue")
	}
}

func TestBudget_ThrottledReleasesAtTheRateInOrder(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	ctx := context.Background()
	var released []<-chan bool
	for i := 0; i < 10; i++ {
		released = append(released, waiter(ctx, h.budget, false))
		waitQueued(t, h.budget, i+1) // enqueue strictly in order
	}

	h.step(time.Second) // 2 per second
	time.Sleep(50 * time.Millisecond)

	got := 0
	for i, ch := range released {
		select {
		case ok := <-ch:
			if !ok {
				t.Fatalf("waiter %d was refused", i)
			}
			if i >= 2 {
				t.Fatalf("waiter %d released out of order", i)
			}
			got++
		default:
		}
	}
	if got != 2 {
		t.Fatalf("released %d in one second at 2/s, want 2", got)
	}
}

func TestBudget_CanceledWaiterConsumesNoToken(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	cctx, cancel := context.WithCancel(context.Background())
	first := waiter(cctx, h.budget, false)
	waitQueued(t, h.budget, 1)
	second := waiter(context.Background(), h.budget, false)
	waitQueued(t, h.budget, 2)

	cancel()
	if ok := <-first; ok {
		t.Fatal("a canceled waiter must be refused")
	}

	h.step(500 * time.Millisecond) // one token at 2/s
	select {
	case ok := <-second:
		if !ok {
			t.Fatal("the live waiter must be admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the canceled waiter ate the token")
	}
}

func TestBudget_BackendFailingCutsTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectTimeoutPlatform, 200)
	h.budget.control()
	if got := h.budget.currentRate(); got != budgetCeilingRate/budgetDecreaseFactor {
		t.Fatalf("rate after one bad interval = %v, want %v", got, budgetCeilingRate/budgetDecreaseFactor)
	}
	for i := 0; i < 10; i++ {
		h.attempts(connectTimeoutPlatform, 50)
		h.budget.control()
	}
	if got := h.budget.currentRate(); got != budgetFloorRate {
		t.Fatalf("rate never goes below the floor, got %v", got)
	}
}

func TestBudget_FewAttemptsDoNotCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectTimeoutPlatform, budgetMinDecreaseAttempts-1)
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("a handful of failures is noise and must not throttle")
	}
}

func TestBudget_DeadProxiesDoNotCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectSocksFail, 500)
	h.attempts(connectOK, 100)
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("dead socks proxies are a proxy-list problem and must not throttle the fleet")
	}
}

func TestBudget_RecoveryRampsBackToOpen(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	for i := 0; i < 20 && h.budget.currentRate() < budgetCeilingRate; i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control()
	}
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatalf("a healthy backend must ramp back to the ceiling, stuck at %v", h.budget.currentRate())
	}
	if h.budget.throttled() {
		t.Fatal("at the ceiling with an empty queue the budget is open again")
	}
}

// The reason the budget exists: when the backend heals, thousands of parked
// proxies must come back fast but never as one herd.
func TestBudget_ThousandsOfParkedProxiesComeBackWithinASecondOrTwoOfTheRamp(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	const parked = 4000
	ctx := context.Background()
	released := make(chan struct{}, parked)
	for i := 0; i < parked; i++ {
		go func() {
			if h.budget.admit(ctx, false) {
				released <- struct{}{}
			}
		}()
	}
	waitQueued(t, h.budget, parked)

	// the backend is healthy from now on
	perSecond := map[int64]int{}
	count := 0
	start := *h.now
	for count < parked && h.now.Sub(start) < 5*time.Minute {
		h.advance(budgetTick)
		if h.now.Sub(start)%budgetControlInterval == 0 {
			h.attempts(connectOK, 20)
			h.budget.control()
		}
		h.budget.tick(*h.now)
		time.Sleep(200 * time.Microsecond) // let released goroutines report
		for drained := false; !drained; {
			select {
			case <-released:
				count++
				perSecond[int64(h.now.Sub(start)/time.Second)]++
			default:
				drained = true
			}
		}
	}
	elapsed := h.now.Sub(start)
	if count != parked {
		t.Fatalf("only %d of %d parked proxies were released in %s", count, parked, elapsed)
	}
	if elapsed > 90*time.Second {
		t.Fatalf("4000 proxies took %s to come back, recovery must be about a minute", elapsed)
	}
	for sec, n := range perSecond {
		if float64(n) > budgetCeilingRate*1.2 {
			t.Fatalf("second %d released %d proxies, above the %v/s ceiling", sec, n, budgetCeilingRate)
		}
	}
}

func TestBudget_ThrottledSinceRateOrQueue(t *testing.T) {
	h := newBudgetHarness()
	if h.budget.throttled() {
		t.Fatal("fresh budget is open")
	}
	h.budget.setRate(budgetFloorRate)
	if !h.budget.throttled() {
		t.Fatal("a reduced rate is throttled")
	}
}

func newProxiedFlapTransport(t *testing.T, ctx context.Context, serverUrl string, settings *PlatformTransportSettings) *PlatformTransport {
	t.Helper()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategySettings.RequestTimeout = 300 * time.Millisecond
	// a proxy that does not exist: the test is about the gate in front of the
	// connect, so the connect itself may fail once it is admitted
	strategySettings.ProxySettings = &ProxySettings{Index: 5, Network: "tcp", Address: "127.0.0.1:1"}
	strategy := NewClientStrategy(ctx, strategySettings)
	routeManager := NewRouteManager(ctx, "test-budget")
	auth := &ClientAuth{ByJwt: "jwt", InstanceId: NewId(), AppVersion: "1.0.0", ProvideIntent: true}
	return NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "ws"+serverUrl[len("http"):], auth, TransportModeH1, settings)
}

// With the budget throttled and enforcing, a proxied transport waits for its
// turn before it starts a connect. Nothing is dialed until it is released.
func TestRunH1_ThrottledBudgetHoldsAProxiedConnect(t *testing.T) {
	defer setGovernModeForTest(governEnforce)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)
	// every third token is random; pin it to the head of the line (a blocker) so
	// the test does not lose about 5% of the time to its own waiter being drawn
	defer connectBudget.setPick(func(n int) int { return 0 })()

	// twenty waiters ahead of us at 2/s: our turn is about ten seconds away
	blockers, releaseBlockers := context.WithCancel(context.Background())
	for i := 0; i < 20; i++ {
		go connectBudget.admit(blockers, false)
	}
	waitQueued(t, connectBudget, 20)

	before := connectGov.snapshot()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newProxiedFlapTransport(t, ctx, "http://127.0.0.1:9", settings)
	defer transport.Close()

	time.Sleep(1500 * time.Millisecond)
	if n := connectGov.snapshot().Attempts - before.Attempts; n != 0 {
		t.Fatalf("a throttled budget let %d connect attempts through", n)
	}

	// release the line: the transport must now get its turn and dial
	connectBudget.setRate(budgetCeilingRate)
	releaseBlockers()
	deadline := time.After(10 * time.Second)
	for connectGov.snapshot().Attempts-before.Attempts == 0 {
		select {
		case <-deadline:
			t.Fatal("transport never got its turn after the budget opened")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The same throttle, but not enforcing: shadow mode must never delay a connect.
func TestRunH1_ShadowModeNeverHoldsAConnect(t *testing.T) {
	defer setGovernModeForTest(governShadow)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)

	before := connectGov.snapshot()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newProxiedFlapTransport(t, ctx, "http://127.0.0.1:9", settings)
	defer transport.Close()

	deadline := time.After(5 * time.Second)
	for connectGov.snapshot().Attempts-before.Attempts == 0 {
		select {
		case <-deadline:
			t.Fatal("shadow mode held a connect")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The direct path is the scout of last resort and never waits.
func TestRunH1_DirectPathIgnoresAThrottledBudget(t *testing.T) {
	defer setGovernModeForTest(governEnforce)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)
	blockers, releaseBlockers := context.WithCancel(context.Background())
	defer releaseBlockers()
	for i := 0; i < 20; i++ {
		go connectBudget.admit(blockers, false)
	}
	waitQueued(t, connectBudget, 20)

	var accepts atomic.Int64
	var stamps flapStamps
	server := flapServer(time.Hour, &accepts, &stamps)
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newFlapTestTransport(t, ctx, server.URL, settings) // no ProxySettings: the direct path
	defer transport.Close()

	deadline := time.After(5 * time.Second)
	for accepts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the direct path waited on the budget")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// While the backend is failing, one failed attempt must not turn into many
// dial rounds over the full request timeout.
func TestWsDialContext_DegradedBackendGetsOneRound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "database is on fire", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newFastFailStrategy(ctx, 800*time.Millisecond)

	prev := singleRoundCheck
	defer func() { singleRoundCheck = prev }()

	singleRoundCheck = func() bool { return true }
	start := time.Now()
	_, _, err := strategy.WsDialContext(ctx, url, http.Header{})
	short := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "1 round(s)") {
		t.Fatalf("a degraded backend should get exactly one round, got %v", err)
	}
	if short > 400*time.Millisecond {
		t.Fatalf("one round took %s, it must not wait out the request timeout", short)
	}

	singleRoundCheck = func() bool { return false }
	start = time.Now()
	_, _, err = strategy.WsDialContext(ctx, url, http.Header{})
	if long := time.Since(start); long < 700*time.Millisecond {
		t.Fatalf("a healthy backend keeps retrying for the request timeout, returned after %s (%v)", long, err)
	}
}

// Scouts that land on dead proxies tell the controller nothing. A throttle that
// only ever sees such attempts must not sit at the floor forever.
func TestBudget_UninformativeAttemptsLiftTheThrottle(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter(ctx, h.budget, false)
	waitQueued(t, h.budget, 1)

	h.attempts(connectSocksFail, 30)
	h.attempts(connectTimeoutNoReply, 30)
	h.budget.control()
	if got := h.budget.currentRate(); got != budgetFloorRate*budgetIncreaseFactor {
		t.Fatalf("rate after an uninformative interval = %v, want %v", got, budgetFloorRate*budgetIncreaseFactor)
	}
}

// If the dispatcher dies, waiters must not park forever.
func TestBudget_FailsOpenWhenTheDispatcherStalls(t *testing.T) {
	defer setBudgetStallForTest(10*time.Millisecond, 50*time.Millisecond)()

	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	out := waiter(context.Background(), h.budget, false)
	waitQueued(t, h.budget, 1)

	h.advance(time.Minute) // time passes and nothing ticks
	select {
	case ok := <-out:
		if !ok {
			t.Fatal("a stalled budget must fail open, not refuse")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter stayed parked behind a dead dispatcher")
	}
}

func TestBudget_HealthyDispatcherDoesNotFailOpen(t *testing.T) {
	defer setBudgetStallForTest(10*time.Millisecond, 50*time.Millisecond)()

	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := waiter(ctx, h.budget, false)
	waitQueued(t, h.budget, 1)

	for i := 0; i < 5; i++ {
		h.advance(10 * time.Millisecond)
		h.budget.tick(*h.now) // the dispatcher is alive, just slow at 2/s
		time.Sleep(15 * time.Millisecond)
	}
	select {
	case <-out:
		t.Fatal("a live dispatcher at its rate must not be bypassed")
	default:
	}
}

// The reaper must stay out of the way for as long as an outage could still be
// the reason a proxy is down, including after the budget has reopened.
func TestBudget_OutageSignalOutlivesTheThrottle(t *testing.T) {
	h := newBudgetHarness()
	if h.budget.outageRecent(time.Hour) {
		t.Fatal("a healthy budget has never seen an outage")
	}

	h.attempts(connectTimeoutPlatform, 200)
	h.budget.control()
	if !h.budget.outageRecent(time.Minute) {
		t.Fatal("a throttled budget is an outage in progress")
	}

	// the backend heals and the budget ramps back to open
	for i := 0; i < 20 && h.budget.throttled(); i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control()
	}
	if h.budget.throttled() {
		t.Fatal("setup: the budget should have reopened")
	}
	if !h.budget.outageRecent(30 * time.Minute) {
		t.Fatal("a proxy that has been down since the outage may only be dead after a long quiet period")
	}
	h.advance(31 * time.Minute)
	if h.budget.outageRecent(30 * time.Minute) {
		t.Fatal("thirty quiet minutes later the outage is over")
	}
}

func scored(score uint64) func() uint64 { return func() uint64 { return score } }

func waiterScored(ctx context.Context, b *reconnectBudget, score uint64) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- b.admitParked(ctx, false, scored(score), nil) }()
	return out
}

// When tokens are scarce the proxies with the best record go first: the ones
// that earned before the outage are the likeliest to work, and they are the ones
// worth having back soonest.
func TestBudget_BestHistoryScoutsGoFirst(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	h.budget.pick = func(n int) int { return 0 }

	scores := []uint64{5, 1, 9, 3, 7, 2}
	outs := make([]<-chan bool, len(scores))
	for i, sc := range scores {
		outs[i] = waiterScored(context.Background(), h.budget, sc)
		waitQueued(t, h.budget, i+1)
	}

	h.step(time.Second) // two tokens
	time.Sleep(50 * time.Millisecond)

	released := map[uint64]bool{}
	for i, ch := range outs {
		select {
		case <-ch:
			released[scores[i]] = true
		default:
		}
	}
	if len(released) != 2 || !released[9] || !released[7] {
		t.Fatalf("the two best (9 and 7) should have gone first, released %v", released)
	}
}

// A third of the slots are random so one stale favourite, or a proxy with no
// history yet, cannot hold everyone else out.
func TestBudget_EveryThirdScoutIsRandom(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(3) // 3 tokens in a second
	h.budget.pick = func(n int) int { return 0 }

	scores := []uint64{1, 9, 8, 7, 6}
	outs := make([]<-chan bool, len(scores))
	for i, sc := range scores {
		outs[i] = waiterScored(context.Background(), h.budget, sc)
		waitQueued(t, h.budget, i+1)
	}

	h.step(time.Second)
	time.Sleep(50 * time.Millisecond)

	released := map[uint64]bool{}
	for i, ch := range outs {
		select {
		case <-ch:
			released[scores[i]] = true
		default:
		}
	}
	// two best-first (9, 8) then one random slot; the injected pick(0) takes the
	// head of what is left, which is the lowest-scored arrival (1)
	if len(released) != 3 || !released[9] || !released[8] || !released[1] {
		t.Fatalf("expected the two best then a random pick, released %v", released)
	}
}

func TestBudget_EqualScoresStayFirstInFirstOut(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	outs := make([]<-chan bool, 5)
	for i := range outs {
		outs[i] = waiterScored(context.Background(), h.budget, 4)
		waitQueued(t, h.budget, i+1)
	}
	h.step(time.Second)
	time.Sleep(50 * time.Millisecond)
	for i, ch := range outs {
		select {
		case <-ch:
			if 2 <= i {
				t.Fatalf("waiter %d released before earlier equal-score arrivals", i)
			}
		default:
			if i < 2 {
				t.Fatalf("waiter %d should have been released first", i)
			}
		}
	}
}

// Priority must never become starvation: with wildly different scores every
// parked proxy still comes back within the ramp.
func TestBudget_LowScoresStillComeBackAtTheRamp(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	const parked = 2000
	released := make(chan struct{}, parked)
	for i := 0; i < parked; i++ {
		score := uint64(i % 97)
		go func() {
			if h.budget.admitParked(context.Background(), false, scored(score), nil) {
				released <- struct{}{}
			}
		}()
	}
	waitQueued(t, h.budget, parked)

	count := 0
	start := *h.now
	for count < parked && h.now.Sub(start) < 5*time.Minute {
		h.advance(budgetTick)
		if h.now.Sub(start)%budgetControlInterval == 0 {
			h.attempts(connectOK, 20)
			h.budget.control()
		}
		h.budget.tick(*h.now)
		time.Sleep(100 * time.Microsecond)
		for drained := false; !drained; {
			select {
			case <-released:
				count++
			default:
				drained = true
			}
		}
	}
	if count != parked || h.now.Sub(start) > 90*time.Second {
		t.Fatalf("released %d of %d in %s", count, parked, h.now.Sub(start))
	}
}

func (h *budgetHarness) failedBytes(class connectClass, n int, bytesEach int64) {
	for i := 0; i < n; i++ {
		st := &attemptStats{}
		st.tx.Store(bytesEach)
		h.gov.recordAttempt(class, st)
	}
}

// The classifier can be wrong about WHY attempts fail. The bytes they burn are
// not an opinion: a flood of failed attempts that the verdict calls ambiguous
// still has to be cut.
func TestBudget_FailedAttemptBytesCutTheRateEvenWhenTheVerdictIsUnsure(t *testing.T) {
	h := newBudgetHarness()
	h.failedBytes(connectTimeoutNoReply, 400, 20_000) // 8 MB of failures in the window
	if h.gov.snapshotWindow(budgetRecentWindow).Degraded {
		t.Fatal("setup: silent timeouts alone must not produce a verdict")
	}
	h.budget.control()
	if got := h.budget.currentRate(); got != budgetCeilingRate/budgetDecreaseFactor {
		t.Fatalf("failed attempts burned %d bytes but the rate is %v", 400*20_000, got)
	}
}

func TestBudget_SuccessfulConnectsNeverTripTheByteBreaker(t *testing.T) {
	h := newBudgetHarness()
	h.failedBytes(connectOK, 2000, 20_000) // a big healthy startup: 40 MB of handshakes
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("a healthy startup spends bytes on connects that succeed and must not be throttled")
	}
}

func TestBudget_CheapFailuresDoNotTripTheByteBreaker(t *testing.T) {
	h := newBudgetHarness()
	h.failedBytes(connectSocksFail, 2000, 300) // dead proxies refuse in a few hundred bytes
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("dead proxies are cheap and must not throttle the fleet")
	}
}

// A restart in the middle of an outage must not stampede: the previous run left
// a marker, and the new one starts throttled and ramps on evidence.
func TestGovernState_RestartDuringAnOutageStartsThrottled(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	h := newBudgetHarness()
	h.attempts(connectTimeoutPlatform, 200)
	h.budget.control()
	h.budget.writeOutageMarker(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a throttled budget must leave a marker: %v", err)
	}

	next := newBudgetHarness()
	next.budget.restoreOutageMarker(path, *next.now)
	if got := next.budget.currentRate(); got != budgetFloorRate {
		t.Fatalf("a restart during an outage must start at the floor, got %v", got)
	}
	if !next.budget.outageRecent(time.Minute) {
		t.Fatal("the restarted process must know an outage was in progress")
	}
}

// A healthy restart or hot swap pays nothing.
func TestGovernState_HealthyRestartStartsOpen(t *testing.T) {
	dir := t.TempDir()
	h := newBudgetHarness()
	h.budget.restoreOutageMarker(dir+"/missing", *h.now)
	if h.budget.currentRate() != budgetCeilingRate || h.budget.throttled() {
		t.Fatal("with no marker the budget starts open")
	}

	stale := dir + "/stale"
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := h.now.Add(-2 * outageMarkerMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	h.budget.restoreOutageMarker(stale, *h.now)
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("a marker from a long time ago is not an outage in progress")
	}
}

// Shadow mode exists to show what enforce would decide, so it restores and
// keeps the learned baseline too. The outage marker stays enforce-only: only
// enforce changes behaviour from it.
func TestGovernState_ShadowRestoresAndKeepsTheBaseline(t *testing.T) {
	defer setGovernModeForTest(governShadow)()
	previousGov, previousBudget := connectGov, connectBudget
	defer func() {
		connectGov, connectBudget = previousGov, previousBudget
	}()
	connectGov = newConnectGovernor(time.Now)
	connectBudget = newReconnectBudget(connectGov, time.Now)
	connectBudget.emit = func(string) {}

	dir := t.TempDir()
	baselinePath := dir + "/govern_baseline"
	if err := os.WriteFile(baselinePath, []byte("0.30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := dir + "/govern_outage"
	if err := os.WriteFile(markerPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	InitGovernState(markerPath)

	if got := connectGov.snapshot().Baseline; got < 0.29 || 0.31 < got {
		t.Fatalf("shadow restored baseline %.2f, want about 0.30", got)
	}
	if connectGov.baselinePath != baselinePath {
		t.Fatalf("shadow baseline path = %q, want %q", connectGov.baselinePath, baselinePath)
	}
	if connectBudget.outageRecent(time.Hour) {
		t.Fatal("shadow restored the outage marker, which changes behaviour")
	}
}

func TestGovernState_MarkerClearsWhenTheBudgetReopens(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	h.budget.markerPath = path
	h.budget.writeOutageMarker(path)

	for i := 0; i < 20 && h.budget.throttled(); i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control()
	}
	if h.budget.throttled() {
		t.Fatal("setup: should have reopened")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the marker must be removed once the budget is open again")
	}
}

// The protection is on for everyone unless an operator explicitly turns it
// down. Off and shadow exist as an emergency override, not as the default.
func TestParseGovernMode_DefaultsToEnforce(t *testing.T) {
	cases := []struct {
		raw  string
		want governMode
	}{
		{"", governEnforce},
		{"   ", governEnforce},
		{"enforce", governEnforce},
		{"ENFORCE", governEnforce},
		{"on", governEnforce},
		{"1", governEnforce},
		{"true", governEnforce},
		{"shadow", governShadow},
		{" Shadow ", governShadow},
		{"off", governOff},
		{"0", governOff},
		{"false", governOff},
		{"no", governOff},
		{"enfroce", governEnforce}, // a typo must not silently disable the protection
	}
	for _, c := range cases {
		if got := parseGovernMode(c.raw); got != c.want {
			t.Errorf("parseGovernMode(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// The budget reacts only to failures the platform visibly produced, for the same
// reason the verdict does.
func TestBudget_FlappingWithoutPlatformFailuresDoesNotCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectOK, 100)
	for i := 0; i < 100; i++ {
		h.gov.recordEarlyDeath()
	}
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("proxy-side flapping must not throttle reconnects")
	}
}

// A healthy startup in which a minority of proxies fail after the TLS handshake
// spends real bytes on those failures. That is not a flood and must not throttle
// the other proxies.
func TestBudget_ByteBreakerNeedsMostAttemptsToBeFailing(t *testing.T) {
	h := newBudgetHarness()
	h.failedBytes(connectOK, 850, 20_000)
	h.failedBytes(connectTimeoutNoReply, 150, 20_000) // 3 MB of failures in the window, 15% of attempts
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("a minority of failing proxies must not trip the byte breaker")
	}
	// but when most attempts fail the bytes are a flood
	storm := newBudgetHarness()
	storm.failedBytes(connectOK, 100, 20_000)
	storm.failedBytes(connectTimeoutNoReply, 400, 20_000)
	storm.budget.control()
	if storm.budget.currentRate() != budgetCeilingRate/budgetDecreaseFactor {
		t.Fatal("when most attempts fail and burn the bytes the breaker must cut")
	}
}

// The marker is a file. File I/O on a slow or stalled disk must never happen
// while the budget lock is held: that lock is also taken (through the backend
// verdict) under the proxy health lock, so a stalled write would stall every
// proxy's health bookkeeping.
func TestGovernState_MarkerIOHappensWithoutTheBudgetLock(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	h := newBudgetHarness()
	h.budget.markerPath = path

	lockFreeDuringIO := 0
	ioCalls := 0
	h.budget.ioProbe = func() {
		ioCalls++
		if h.budget.mu.TryLock() {
			lockFreeDuringIO++
			h.budget.mu.Unlock()
		}
	}

	h.attempts(connectTimeoutPlatform, 200)
	h.attempts(connectUpgradeFail, 5)
	h.budget.control() // throttles and writes the marker
	for i := 0; i < 20 && h.budget.throttled(); i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control() // eventually reopens and removes the marker
	}
	if ioCalls < 2 {
		t.Fatalf("setup: expected a write and a remove, saw %d file operations", ioCalls)
	}
	if lockFreeDuringIO != ioCalls {
		t.Fatalf("the budget lock was held during %d of %d file operations", ioCalls-lockFreeDuringIO, ioCalls)
	}
}

// Under real memory pressure every reconnect attempt (TLS state, buffers,
// goroutines) makes it worse; during the 2026-10 outage one box sat at 20 of
// 24 GB. The budget holds reconnects down then, even while the backend is
// fine. This is a host problem, not a backend one: it must never read as an
// outage (that freezes the reaper, blocks renewal and writes a marker that
// slows the next start).
func TestBudget_MemoryPressureShedsReconnectsAsAHostProblemNotAnOutage(t *testing.T) {
	h := newBudgetHarness()
	pressured := true
	h.budget.memoryPressure = func() bool { return pressured }
	h.attempts(connectOK, 100) // the backend is perfectly healthy
	path := t.TempDir() + "/govern_outage"
	h.budget.markerPath = path

	for i := 0; i < budgetMemoryConsecutive-1; i++ {
		h.budget.control()
	}
	if h.budget.effectiveRate() != budgetCeilingRate {
		t.Fatal("one or two pressured samples are noise and must not shed")
	}
	h.budget.control()
	if got := h.budget.effectiveRate(); got != budgetCeilingRate/budgetDecreaseFactor {
		t.Fatalf("sustained pressure must cut the rate: %v", got)
	}
	for i := 0; i < 10; i++ {
		h.budget.control()
	}
	if got := h.budget.effectiveRate(); got != budgetMemoryFloorRate {
		t.Fatalf("sustained pressure bottoms at the startup-safe floor %v, got %v", budgetMemoryFloorRate, got)
	}

	// but it is NOT a backend outage
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("the backend verdict's rate must be untouched by memory pressure")
	}
	if h.budget.outageRecent(time.Hour) {
		t.Fatal("memory pressure must not read as a backend outage")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("memory pressure must not write the outage marker (it would throttle the next start)")
	}

	// pressure gone: it recovers
	pressured = false
	for i := 0; i < 40 && h.budget.effectiveRate() < budgetCeilingRate; i++ {
		h.budget.control()
	}
	if h.budget.effectiveRate() != budgetCeilingRate {
		t.Fatal("must reopen once the pressure is gone")
	}
}

func TestBudget_PressureReleasesRealTokensAtTheShedRate(t *testing.T) {
	h := newBudgetHarness()
	h.budget.memoryPressure = func() bool { return true }
	for i := 0; i < 12; i++ {
		h.budget.control()
	}
	waiters := 200
	released := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			if h.budget.admit(context.Background(), false) {
				released <- struct{}{}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	h.step(2 * time.Second)
	time.Sleep(50 * time.Millisecond)
	got := len(released)
	if got < int(budgetMemoryFloorRate)*2-3 || int(budgetMemoryFloorRate)*2+3 < got {
		t.Fatalf("released %d waiters in 2s under pressure, want about %d", got, int(budgetMemoryFloorRate)*2)
	}
}

func TestMemoryPressureFrom_ThresholdsAndSources(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		name         string
		avail, total uint64
		want         bool
	}{
		{"roomy", 20 * gib, 24 * gib, false},
		{"nearly out of memory", 1 * gib, 24 * gib, true},
		{"tight but not critical", 3 * gib, 24 * gib, false},
		{"unknown memory", 0, 0, false},
	}
	for _, c := range cases {
		if got := memoryPressureFrom(c.avail, c.total); got != c.want {
			t.Errorf("%s: pressure=%v, want %v", c.name, got, c.want)
		}
	}
}

// The Go heap sitting near GOMEMLIMIT is the runtime working as designed (turbo
// sets GOGC=200 under a limit of 80% of RAM, and GC-off grows to the limit), not
// a reason to shed reconnects.
func TestMemoryPressure_HeapNearTheGoLimitIsNotPressure(t *testing.T) {
	const gib = 1 << 30
	if memoryPressureFrom(20*gib, 24*gib) {
		t.Fatal("plenty of host memory available must not read as pressure, whatever the heap does")
	}
}

// The rate opens when the host is doing about as well as it usually does, not only
// when 50% of attempts succeed. A host whose normal is 30% good would otherwise sit
// at the floor for ever after one outage: the cut needs a collapse and the open
// needs more than the host ever achieves, and a held budget freezes the reaper and
// ignores real renewal refusals.
func TestBudget_ChronicallyPoorHostReopensAtItsOwnNormal(t *testing.T) {
	h := newBudgetHarness()
	for i := 0; i < 10; i++ {
		h.attempts(connectOK, 9)
		h.attempts(connectTimeoutPlatform, 21) // 30% good
		h.gov.learnBaseline()
		h.advance(61 * time.Second)
	}
	h.attempts(connectTimeoutPlatform, 60) // an outage
	h.budget.control()
	if h.budget.currentRate() == budgetCeilingRate {
		t.Fatal("setup: the collapse must cut the rate")
	}
	h.advance(61 * time.Second)

	for i := 0; i < 30 && h.budget.currentRate() < budgetCeilingRate; i++ {
		h.attempts(connectOK, 4)
		h.attempts(connectTimeoutPlatform, 8) // 33% good: its normal
		h.budget.control()
		h.advance(budgetControlInterval)
	}
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatalf("the budget stayed at %v although the host is doing as well as ever", h.budget.currentRate())
	}
}

// Holding reconnects back for memory is a host problem. It must never read as a
// backend outage, or a memory-pressured box freezes its reaper, ignores real
// renewal refusals, and writes a marker that makes its OOM restart start at the
// backend floor instead of the memory floor.
func TestBudget_AMemoryQueueIsNotAnOutage(t *testing.T) {
	h := newBudgetHarness()
	path := t.TempDir() + "/govern_outage"
	h.budget.markerPath = path
	h.budget.memoryPressure = func() bool { return true }
	for i := 0; i < 8; i++ {
		h.budget.control()
	}
	// a proxy waits behind the memory cap
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.budget.admit(ctx, false)
	time.Sleep(50 * time.Millisecond)
	h.budget.control()

	if h.budget.throttled() {
		t.Fatal("a memory-held queue read as a throttled (outage) budget")
	}
	if h.budget.outageRecent(time.Hour) {
		t.Fatal("a memory-held queue read as a recent outage")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a memory-held queue wrote the outage marker")
	}
}

// An operator reading the log must be able to see when the budget changed, why,
// and on what evidence, not only that the verdict flipped.
type budgetEvents struct {
	mu    sync.Mutex
	lines []string
}

func (e *budgetEvents) emit(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, s)
}

func (e *budgetEvents) joined() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

func TestBudget_LogsEachCutAndTheReopenWithTheEvidence(t *testing.T) {
	h := newBudgetHarness()
	ev := &budgetEvents{}
	h.budget.emitAt = nil
	h.budget.emit = ev.emit
	h.attempts(connectTimeoutPlatform, 60)
	h.budget.control()
	got := ev.joined()
	for _, want := range []string{"[govern] budget cut", "200.0/s to 50.0/s", "reason=verdict", "good=0%", "hard_failures=60"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cut line is missing %q:\n%s", want, got)
		}
	}
	h.advance(61 * time.Second)
	for i := 0; i < 20 && h.budget.currentRate() < budgetCeilingRate; i++ {
		h.attempts(connectOK, 30)
		h.budget.control()
		h.advance(budgetControlInterval)
	}
	if got := ev.joined(); !strings.Contains(got, "[govern] budget reopened") || !strings.Contains(got, "throttled for") {
		t.Fatalf("no reopen line:\n%s", got)
	}
}

func TestBudget_ByteBreakerCutNamesItsReason(t *testing.T) {
	h := newBudgetHarness()
	ev := &budgetEvents{}
	h.budget.emitAt = nil
	h.budget.emit = ev.emit
	h.failedBytes(connectOK, 100, 20_000)
	h.failedBytes(connectTimeoutNoReply, 400, 20_000)
	h.budget.control()
	if got := ev.joined(); !strings.Contains(got, "reason=bytes") || !strings.Contains(got, "failed_KB/s=") {
		t.Fatalf("byte breaker cut is not explained:\n%s", got)
	}
}

func TestBudget_LogsMemoryPressureStartAndEnd(t *testing.T) {
	h := newBudgetHarness()
	ev := &budgetEvents{}
	h.budget.emitAt = nil
	h.budget.emit = ev.emit
	pressured := true
	h.budget.memoryPressure = func() bool { return pressured }
	for i := 0; i < budgetMemoryConsecutive+1; i++ {
		h.budget.control()
	}
	if got := ev.joined(); !strings.Contains(got, "[govern] memory pressure: holding reconnects") {
		t.Fatalf("no memory pressure line:\n%s", got)
	}
	for i := 0; i < 10; i++ {
		h.budget.control()
	}
	if n := strings.Count(ev.joined(), "memory pressure: holding"); n != 1 {
		t.Fatalf("the start of pressure is one line, not one per tick: %d\n%s", n, ev.joined())
	}
	pressured = false
	for i := 0; i < 30 && h.budget.effectiveRate() < budgetCeilingRate; i++ {
		h.budget.control()
	}
	if got := ev.joined(); !strings.Contains(got, "[govern] memory pressure over") {
		t.Fatalf("no end-of-pressure line:\n%s", got)
	}
}

func TestBudget_LogsAFoundRestartMarker(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newBudgetHarness()
	ev := &budgetEvents{}
	h.budget.emitAt = nil
	h.budget.emit = ev.emit
	h.budget.restoreOutageMarker(path, time.Now())
	if got := ev.joined(); !strings.Contains(got, "[govern] restart marker found") || !strings.Contains(got, "starting at 2.0/s") {
		t.Fatalf("no marker line:\n%s", got)
	}
}

// The same stampede must not make the budget cut a start.
func TestBudget_DoesNotCutDuringTheStartupGrace(t *testing.T) {
	h := newBudgetHarness()
	h.gov.started = *h.now // only just started
	h.attempts(connectTimeoutPlatform, 300)
	h.failedBytes(connectTimeoutNoReply, 900, 20_000) // and the byte breaker would trip too
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatalf("the budget cut a start: %v", h.budget.currentRate())
	}
	h.advance(govStartupGrace + time.Second)
	h.attempts(connectTimeoutPlatform, 300)
	h.failedBytes(connectTimeoutNoReply, 900, 20_000)
	h.budget.control()
	if h.budget.currentRate() == budgetCeilingRate {
		t.Fatal("after the grace the same failures must cut the rate")
	}
}

// A log write or a marker write that blocks on a stalled disk must not stop the
// dispatcher: a stale tick makes every parked waiter look stalled and releases
// them all, at the moment the budget has just been cut for a failing backend.
func TestBudget_BlockedSideEffectsDoNotStallTheDispatcher(t *testing.T) {
	h := newBudgetHarness()
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	h.budget.emitAt = nil
	h.budget.emit = func(string) {
		started <- struct{}{}
		<-release // a stuck disk
	}
	h.budget.markerPath = t.TempDir() + "/govern_outage"
	h.budget.ioProbe = func() { <-release }

	h.attempts(connectTimeoutPlatform, 200)
	done := make(chan struct{})
	go func() {
		h.budget.controlWith(h.budget.enqueueSide)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the control decision blocked on its side effects")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never ran the side effect")
	}
	// the dispatcher is free: ticks advance and the budget does not look stalled
	h.advance(budgetTick)
	h.budget.tick(h.budget.now())
	if h.budget.stalled() {
		t.Fatal("the dispatcher looks stalled while only its side effects are stuck")
	}
	if got := h.budget.currentRate(); got >= budgetCeilingRate {
		t.Fatalf("the cut itself must still happen immediately, rate = %v", got)
	}
	close(release)
	// let the worker finish its marker write before the temp dir is removed
	drained := make(chan struct{})
	h.budget.enqueueSide(func() { close(drained) })
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never drained after the stall cleared")
	}
}

func TestBudget_SideEffectQueueDropsAndCountsWhenFull(t *testing.T) {
	h := newBudgetHarness()
	release := make(chan struct{})
	var ran atomic.Int64
	started := make(chan struct{}, 1)
	h.budget.enqueueSide(func() { started <- struct{}{}; <-release })
	<-started // the worker is now stuck inside the first effect
	for i := 0; i < budgetSideQueueLen+10; i++ {
		h.budget.enqueueSide(func() { ran.Add(1) })
	}
	if got := h.budget.sideDropped.Load(); got != 10 {
		t.Fatalf("dropped = %d, want 10", got)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for ran.Load() < budgetSideQueueLen && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ran.Load() != budgetSideQueueLen {
		t.Fatalf("ran = %d, want the %d queued effects", ran.Load(), budgetSideQueueLen)
	}
}

// The attempt cap's transitions are logged from the same control step. They must
// go through the side-effect worker too, or a blocked log write still stalls the
// dispatcher.
func TestBudget_AttemptCapLogLinesDoNotStallTheDispatcher(t *testing.T) {
	h, _ := newCapHarness(4000)
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	h.budget.emit = func(string) {
		started <- struct{}{}
		<-release
	}
	storm := func() {
		h.attempts(connectSocksFail, 1800)
		h.attempts(connectOK, 200)
		h.advance(30 * time.Second)
		h.budget.controlWith(h.budget.enqueueSide)
		h.advance(30 * time.Second)
	}
	done := make(chan struct{})
	go func() {
		storm()
		storm() // the second sustained storm minute engages the cap and logs it
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the control step blocked on a log write")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("no log line ever reached the worker")
	}
	if h.budget.currentCap() == 0 {
		t.Fatal("the cap must engage regardless of the log write")
	}
	close(release)
	drained := make(chan struct{})
	h.budget.enqueueSide(func() { close(drained) })
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never drained")
	}
}

// A dropped side effect (full queue) must not leave a stale marker: the marker
// file is state, not a log line, so the next decision re-applies it.
func TestBudget_DroppedMarkerRemovalIsRetried(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	h := newBudgetHarness()
	h.budget.markerPath = path
	h.attempts(connectTimeoutPlatform, 200)
	h.attempts(connectUpgradeFail, 5)
	h.budget.control() // throttles and writes the marker
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("setup: the marker was not written: %v", err)
	}

	dropped := func(func()) bool { return false }
	for i := 0; i < 30 && h.budget.throttled(); i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.controlWith(dropped) // every effect is dropped, including the removal
	}
	if h.budget.throttled() {
		t.Fatal("setup: the budget never reopened")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("setup: the removal should have been dropped, leaving the marker: %v", err)
	}
	// the next decision, with nothing else to report, removes it
	h.advance(budgetControlInterval)
	h.budget.control()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the stale marker was never removed (stat err = %v)", err)
	}
}

func TestBudget_DroppedMarkerWriteIsRetriedBeforeTheRefreshInterval(t *testing.T) {
	path := t.TempDir() + "/govern_outage"
	h := newBudgetHarness()
	h.budget.markerPath = path
	h.attempts(connectTimeoutPlatform, 200)
	h.attempts(connectUpgradeFail, 5)
	h.budget.controlWith(func(func()) bool { return false }) // the first write is dropped
	if _, err := os.Stat(path); err == nil {
		t.Fatal("setup: the write should have been dropped")
	}
	h.advance(time.Second) // far less than outageMarkerWriteEvery
	h.attempts(connectTimeoutPlatform, 50)
	h.budget.control()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a throttled budget must have its marker after a dropped write: %v", err)
	}
}

// A line keeps the time its decision was made, not the time a late worker got to it.
func TestBudget_QueuedLinesKeepTheirDecisionTime(t *testing.T) {
	h := newBudgetHarness()
	var gotAt time.Time
	var gotLine string
	h.budget.emitAt = func(line string, at time.Time) { gotLine, gotAt = line, at }
	h.attempts(connectTimeoutPlatform, 200)

	var deferred func()
	decided := h.budget.now()
	h.budget.controlWith(func(f func()) bool { deferred = f; return true })
	if deferred == nil {
		t.Fatal("the decision produced no side effect")
	}
	h.advance(10 * time.Minute) // the worker runs late
	deferred()
	if gotLine == "" {
		t.Fatal("no line emitted")
	}
	if !gotAt.Equal(decided) {
		t.Fatalf("line stamped %v, want the decision time %v", gotAt, decided)
	}
}
