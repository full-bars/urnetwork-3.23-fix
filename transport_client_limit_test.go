package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// testingClientLimitTimer is one expiry armed on the manual clock.
type testingClientLimitTimer struct {
	fireTime time.Time
	f        func()
	stopped  bool
	fired    bool
}

// testingClientLimitClock is a manual clock for a ClientLimitBackoff.
type testingClientLimitClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*testingClientLimitTimer
	// every armed timeout, in arm order
	armedTimeouts chan time.Duration
}

// newTestingClientLimitClock starts the clock at a fixed synthetic time.
func newTestingClientLimitClock() *testingClientLimitClock {
	return &testingClientLimitClock{
		now:           time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		armedTimeouts: make(chan time.Duration, 64),
	}
}

// Now is the clock's current time.
func (self *testingClientLimitClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// AfterFunc arms f to run when advance passes timeout, records the timeout,
// and returns the stop, as time.AfterFunc does.
func (self *testingClientLimitClock) AfterFunc(timeout time.Duration, f func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	timer := &testingClientLimitTimer{
		fireTime: self.now.Add(timeout),
		f:        f,
	}
	self.timers = append(self.timers, timer)
	select {
	case self.armedTimeouts <- timeout:
	default:
	}
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if timer.stopped || timer.fired {
			return false
		}
		timer.stopped = true
		return true
	}
}

// advance moves the clock and runs every live timer that came due, outside
// the clock lock, in arm order.
func (self *testingClientLimitClock) advance(d time.Duration) {
	dueTimers := func() []*testingClientLimitTimer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(d)
		dueTimers := []*testingClientLimitTimer{}
		for _, timer := range self.timers {
			if !timer.stopped && !timer.fired && !timer.fireTime.After(self.now) {
				timer.fired = true
				dueTimers = append(dueTimers, timer)
			}
		}
		return dueTimers
	}()
	for _, timer := range dueTimers {
		timer.f()
	}
}

// timer returns the i-th armed timer.
func (self *testingClientLimitClock) timer(i int) *testingClientLimitTimer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.timers[i]
}

// nextArmedTimeout is the timeout of the next arm, or fails.
func (self *testingClientLimitClock) nextArmedTimeout(t *testing.T) time.Duration {
	t.Helper()
	select {
	case timeout := <-self.armedTimeouts:
		return timeout
	case <-time.After(10 * time.Second):
		t.Fatal("no client limit expiry was armed")
		return 0
	}
}

// newTestingClientLimitBackoff is a hold on the manual clock with a fixed
// jitter.
func newTestingClientLimitBackoff(clock *testingClientLimitClock, jitter time.Duration) *ClientLimitBackoff {
	backoff := NewClientLimitBackoff()
	backoff.now = clock.Now
	backoff.afterFunc = clock.AfterFunc
	backoff.jitter = func(maxJitter time.Duration) time.Duration {
		if jitter < maxJitter {
			return jitter
		}
		return maxJitter
	}
	return backoff
}

// noteTestingClientLimitExceeded starts or extends a hold for a close of a
// connection dialed under the current reset generation, which always holds.
func noteTestingClientLimitExceeded(t *testing.T, backoff *ClientLimitBackoff) time.Time {
	t.Helper()
	retryTime, held := backoff.noteExceeded(backoff.dialResetGeneration())
	if !held {
		t.Fatal("a close dialed under the current reset generation started no hold")
	}
	return retryTime
}

// clientLimitNotified reads, without waiting, whether a monitor channel fired.
func clientLimitNotified(notify chan struct{}) bool {
	select {
	case <-notify:
		return true
	default:
		return false
	}
}

// 1. A client limit close holds for the timeout plus jitter, never less than
// ClientLimitBackoffTimeout. The hold ends only when its retry time has come,
// and every change notifies.
func TestClientLimitBackoffHoldsForAtLeastTheTimeout(t *testing.T) {
	if ClientLimitBackoffTimeout < 15*time.Minute {
		t.Fatalf("hold timeout = %s, want at least 15m", ClientLimitBackoffTimeout)
	}
	clock := newTestingClientLimitClock()
	start := clock.Now()
	backoff := newTestingClientLimitBackoff(clock, 2*time.Minute)

	status, notify := backoff.Get()
	if status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("initial status = %+v, want no hold", status)
	}

	retryTime := noteTestingClientLimitExceeded(t, backoff)
	if want := start.Add(ClientLimitBackoffTimeout + 2*time.Minute); !retryTime.Equal(want) {
		t.Fatalf("retry time = %s, want %s", retryTime, want)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("starting the hold did not notify")
	}
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(retryTime) {
		t.Fatalf("status = %+v, want exceeded until %s", status, retryTime)
	}
	if timeout := clock.nextArmedTimeout(t); timeout != ClientLimitBackoffTimeout+2*time.Minute {
		t.Fatalf("armed expiry = %s, want %s", timeout, ClientLimitBackoffTimeout+2*time.Minute)
	}

	// a timer that fires before the retry time keeps the hold and re-arms for
	// the rest
	_, notify = backoff.Get()
	clock.advance(time.Minute)
	clock.timer(0).f()
	if !backoff.Status().Exceeded || clientLimitNotified(notify) {
		t.Fatalf("an early expiry ended or touched the hold: %+v", backoff.Status())
	}
	if timeout := clock.nextArmedTimeout(t); timeout != ClientLimitBackoffTimeout+time.Minute {
		t.Fatalf("re-armed expiry = %s, want %s", timeout, ClientLimitBackoffTimeout+time.Minute)
	}

	// a minute short of the retry time still holds
	clock.advance(ClientLimitBackoffTimeout)
	if !backoff.Status().Exceeded {
		t.Fatal("the hold ended before its retry time")
	}
	clock.advance(time.Minute)
	if status := backoff.Status(); status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("status after the retry time = %+v, want no hold", status)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("the end of the hold did not notify")
	}
}

// 2. A second close inside a hold never shortens it, and extends it when it would
// end later. A timer of an older arm can neither end nor re-arm the newer hold.
func TestClientLimitBackoffRepeatedCloseNeverShortensTheHold(t *testing.T) {
	clock := newTestingClientLimitClock()
	start := clock.Now()
	backoff := newTestingClientLimitBackoff(clock, ClientLimitBackoffJitter)
	first := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)

	// one minute later with no jitter would end four minutes earlier
	backoff.jitter = func(time.Duration) time.Duration { return 0 }
	clock.advance(time.Minute)
	if again := noteTestingClientLimitExceeded(t, backoff); !again.Equal(first) {
		t.Fatalf("a second close moved the retry time from %s to %s", first, again)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a close that ends earlier re-armed the expiry (%s)", timeout)
	default:
	}

	// fourteen minutes into the hold a close ends later and extends it
	clock.advance(13 * time.Minute)
	extended := noteTestingClientLimitExceeded(t, backoff)
	if want := start.Add(14*time.Minute + ClientLimitBackoffTimeout); !extended.Equal(want) {
		t.Fatalf("extended retry time = %s, want %s", extended, want)
	}
	clock.nextArmedTimeout(t)

	// the first arm timer firing late, after the re-arm stopped it
	clock.advance(first.Sub(clock.Now()))
	clock.timer(0).f()
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(extended) {
		t.Fatalf("a stale expiry changed the hold to %+v", status)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a stale expiry re-armed (%s)", timeout)
	default:
	}

	clock.advance(extended.Sub(clock.Now()))
	if backoff.Status().Exceeded {
		t.Fatal("the extended hold did not end at its retry time")
	}

	// a close that ends exactly with the hold in force arms nothing more
	same := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)
	if again := noteTestingClientLimitExceeded(t, backoff); !again.Equal(same) {
		t.Fatalf("an equal close moved the retry time from %s to %s", same, again)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("an equal close re-armed the expiry (%s)", timeout)
	default:
	}
}

// 3. Reset ends a hold at once and notifies; the expiry it stopped can no longer
// touch a hold that starts later.
func TestClientLimitBackoffResetEndsTheHold(t *testing.T) {
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)

	_, notify := backoff.Get()
	backoff.Reset()
	if status := backoff.Status(); status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("status after reset = %+v, want no hold", status)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("the reset did not notify")
	}

	// a new hold, then the reset hold expiry firing late
	clock.advance(time.Minute)
	retryTime := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)
	clock.timer(0).f()
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(retryTime) {
		t.Fatalf("the reset hold expiry changed the new hold to %+v", status)
	}

	// a reset with no hold in force changes nothing
	backoff.Reset()
	_, notify = backoff.Get()
	backoff.Reset()
	if clientLimitNotified(notify) {
		t.Fatal("a reset with no hold notified")
	}
}

// 3b. A reset supersedes every connection dialed before it: a close of one starts
// no hold and changes nothing, and a close dialed after it holds as usual.
func TestClientLimitBackoffResetSupersedesEarlierDials(t *testing.T) {
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	before := backoff.dialResetGeneration()
	backoff.Reset()

	_, notify := backoff.Get()
	if retryTime, held := backoff.noteExceeded(before); held || !retryTime.IsZero() {
		t.Fatalf("a close dialed before the reset held until %s", retryTime)
	}
	if backoff.Status().Exceeded || clientLimitNotified(notify) {
		t.Fatalf("a close dialed before the reset changed the hold to %+v", backoff.Status())
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a close dialed before the reset armed an expiry (%s)", timeout)
	default:
	}

	after := backoff.dialResetGeneration()
	if after == before {
		t.Fatal("the reset did not advance the generation a dial carries")
	}
	if _, held := backoff.noteExceeded(after); !held || !backoff.Status().Exceeded {
		t.Fatalf("a close dialed after the reset did not hold: %+v", backoff.Status())
	}
}

// 4. The production jitter stays inside its bound.
func TestClientLimitBackoffJitterBound(t *testing.T) {
	backoff := NewClientLimitBackoff()
	for range 1000 {
		jitter := backoff.jitter(ClientLimitBackoffJitter)
		if jitter < 0 || ClientLimitBackoffJitter <= jitter {
			t.Fatalf("jitter = %s, want in [0, %s)", jitter, ClientLimitBackoffJitter)
		}
	}
	if jitter := backoff.jitter(0); jitter != 0 {
		t.Fatalf("jitter without a bound = %s, want 0", jitter)
	}
}

// 5. Restore: future sets RetryTime and arms remainder; past no-op; far future clamps.
func TestClientLimitBackoffRestore(t *testing.T) {
	// past no-op
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	past := clock.Now().Add(-time.Minute)
	backoff.Restore(past)
	if status := backoff.Status(); status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("restore in the past set hold: %+v", status)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("restore in the past armed an expiry (%s)", timeout)
	default:
	}

	// future sets RetryTime and arms remainder
	future := clock.Now().Add(10 * time.Minute)
	backoff.Restore(future)
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(future) {
		t.Fatalf("status = %+v, want exceeded until %s", status, future)
	}
	if timeout := clock.nextArmedTimeout(t); timeout != 10*time.Minute {
		t.Fatalf("armed expiry = %s, want 10m", timeout)
	}

	// far future clamps to now + Timeout + Jitter
	clock2 := newTestingClientLimitClock()
	backoff2 := newTestingClientLimitBackoff(clock2, 0)
	farFuture := clock2.Now().Add(24 * time.Hour)
	backoff2.Restore(farFuture)
	wantClamped := clock2.Now().Add(ClientLimitBackoffTimeout + ClientLimitBackoffJitter)
	if status := backoff2.Status(); !status.Exceeded || !status.RetryTime.Equal(wantClamped) {
		t.Fatalf("status = %+v, want clamped to %s", status, wantClamped)
	}
	if timeout := clock2.nextArmedTimeout(t); timeout != ClientLimitBackoffTimeout+ClientLimitBackoffJitter {
		t.Fatalf("armed expiry = %s, want %s", timeout, ClientLimitBackoffTimeout+ClientLimitBackoffJitter)
	}

	// if not later than current hold, ignore
	backoff2.Restore(clock2.Now().Add(5 * time.Minute))
	if status := backoff2.Status(); !status.RetryTime.Equal(wantClamped) {
		t.Fatalf("restore earlier than current shortened hold: %+v", status)
	}
}

// 6. Only a 5-byte close control is a close, and only reason 1 is the client
// limit; other close codes and remote causes are ordinary closes.
func TestClientLimitCloseRecognition(t *testing.T) {
	for _, c := range []struct {
		message []byte
		reason  uint32
		ok      bool
	}{
		{message: []byte{TransportControlClose, 0, 0, 0, 1}, reason: TransportCloseReasonClientLimitExceeded, ok: true},
		{message: []byte{TransportControlClose, 0, 0, 0, 2}, reason: 2, ok: true},
		{message: []byte{TransportControlClose, 0, 0, 1, 0}, reason: 256, ok: true},
		{message: []byte{TransportControlSpeedStart, 0, 0, 0, 1}, ok: false},
		{message: []byte{TransportControlClose, 0, 0, 1}, ok: false},
		{message: []byte{TransportControlClose, 0, 0, 0, 1, 0}, ok: false},
		{message: nil, ok: false},
	} {
		reason, ok := transportCloseReason(c.message)
		if reason != c.reason || ok != c.ok {
			t.Errorf("close reason of %v = (%d, %t), want (%d, %t)", c.message, reason, ok, c.reason, c.ok)
		}
	}

	for _, c := range []struct {
		err  error
		want bool
	}{
		{err: &websocket.CloseError{Code: ClientLimitCloseCode, Text: ClientLimitCloseText}, want: true},
		{err: fmt.Errorf("read: %w", &websocket.CloseError{Code: ClientLimitCloseCode}), want: true},
		{err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, want: false},
		{err: &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode, ErrorMessage: ClientLimitCloseText}, want: true},
		{err: fmt.Errorf("stream: %w", &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode}), want: true},
		// the client own close with the code is not the platform
		{err: &quic.ApplicationError{Remote: false, ErrorCode: ClientLimitCloseCode}, want: false},
		{err: &quic.ApplicationError{Remote: true, ErrorCode: 0}, want: false},
		{err: errors.New(ClientLimitCloseText), want: false},
		{err: nil, want: false},
	} {
		if got := isClientLimitCloseError(c.err); got != c.want {
			t.Errorf("client limit close of %v = %t, want %t", c.err, got, c.want)
		}
	}
}

// 7. TestApplyProvideIntentHeaderOnlyWhenSet
func TestApplyProvideIntentHeaderOnlyWhenSet(t *testing.T) {
	headerWithout := http.Header{}
	applyProvideIntentHeader(headerWithout, ClientAuth{ProvideIntent: false})
	if got := headerWithout.Get(HeaderProvideIntent); got != "" {
		t.Fatalf("got %q, want empty", got)
	}

	headerWith := http.Header{}
	applyProvideIntentHeader(headerWith, ClientAuth{ProvideIntent: true})
	if got := headerWith.Get(HeaderProvideIntent); got != ProvideIntentDeclared {
		t.Fatalf("got %q, want %q", got, ProvideIntentDeclared)
	}
}

// 8. TestAuthProvideIntentRoundTrip (EncodeFrame/DecodeFrame keeps 7) and
// TestH3DatagramValidateAcceptsResponseWithoutProvideIntent.
func TestAuthProvideIntentRoundTrip(t *testing.T) {
	for _, provideIntent := range []bool{false, true} {
		auth := &protocol.Auth{
			ByJwt:         "jwt-token",
			AppVersion:    "1.0.0",
			InstanceId:    []byte("test-instance"),
			ProvideIntent: provideIntent,
		}
		data, err := EncodeFrame(auth, DefaultProtocolVersion)
		if err != nil {
			t.Fatalf("intent %t: EncodeFrame error: %v", provideIntent, err)
		}
		decodedMsg, err := DecodeFrame(data)
		MessagePoolReturn(data)
		if err != nil {
			t.Fatalf("intent %t: DecodeFrame error: %v", provideIntent, err)
		}
		decodedAuth, ok := decodedMsg.(*protocol.Auth)
		if !ok {
			t.Fatalf("intent %t: expected *protocol.Auth, got %T", provideIntent, decodedMsg)
		}
		if decodedAuth.ProvideIntent != provideIntent {
			t.Fatalf("intent %t: got ProvideIntent %t", provideIntent, decodedAuth.ProvideIntent)
		}
	}
}

func TestH3DatagramValidateAcceptsResponseWithoutProvideIntent(t *testing.T) {
	request := &protocol.Auth{
		ByJwt:             "test-jwt",
		AppVersion:        "1.0.0",
		InstanceId:        []byte("instance-1"),
		H3DatagramVersion: H3DatagramProtocolVersion,
		ProvideIntent:     true,
	}
	response := &protocol.Auth{
		ByJwt:                     "test-jwt",
		AppVersion:                "1.0.0",
		InstanceId:                []byte("instance-1"),
		H3DatagramVersion:         H3DatagramProtocolVersion,
		H3DatagramAcceptedVersion: H3DatagramProtocolVersion,
		ProvideIntent:             false,
	}
	accepted, err := ValidateH3DatagramAuthResponse(request, response, true, true, true)
	if err != nil {
		t.Fatalf("expected validation success without provide intent in response, got error: %v", err)
	}
	if !accepted {
		t.Fatal("expected datagram accepted to be true")
	}
}

// 9. TestPlatformTransportH1CloseControlHoldsDials: httptest ws server sends [3,0,0,0,1];
// hold exceeded; clientLimitHoldForTest fires; accept count stays 1. Reason 2 and close 1000: no hold.
func TestPlatformTransportH1CloseControlHoldsDials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)

	parked := make(chan struct{}, 16)
	var acceptCount atomic.Int64

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := acceptCount.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		if count == 1 {
			// send close control with Reason 1 (client limit exceeded)
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{byte(TransportControlClose), 0, 0, 0, 1})
			// read until client closes or EOF
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	serverUrl := "ws" + strings.TrimPrefix(server.URL, "http")

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 5 * time.Millisecond
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}

	strategy := NewClientStrategyWithDefaults(ctx)
	routeManager := NewRouteManager(ctx, "test-h1-hold")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}

	transport := NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, serverUrl, auth, TransportModeH1, settings)
	defer transport.Close()

	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("transport did not park on client limit hold")
	}

	if count := acceptCount.Load(); count != 1 {
		t.Fatalf("expected acceptCount 1, got %d", count)
	}
	if !backoff.Status().Exceeded {
		t.Fatal("expected backoff status Exceeded to be true")
	}

	// Reason 2: no hold
	func() {
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		backoff2 := newTestingClientLimitBackoff(newTestingClientLimitClock(), 0)
		parked2 := make(chan struct{}, 16)
		var acceptCount2 atomic.Int64

		server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := acceptCount2.Add(1)
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			if count == 1 {
				// send close control with Reason 2 (other reason)
				_ = ws.WriteMessage(websocket.BinaryMessage, []byte{byte(TransportControlClose), 0, 0, 0, 2})
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			}
		}))
		defer server2.Close()

		settings2 := DefaultPlatformTransportSettings()
		settings2.ReconnectTimeout = 5 * time.Millisecond
		settings2.ClientLimitBackoff = backoff2
		settings2.clientLimitHoldForTest = func() {
			select {
			case parked2 <- struct{}{}:
			default:
			}
		}

		strategy2 := NewClientStrategyWithDefaults(ctx2)
		rm2 := NewRouteManager(ctx2, "test-h1-reason2")
		t2 := NewPlatformTransportWithTargetMode(ctx2, strategy2, rm2, "ws"+strings.TrimPrefix(server2.URL, "http"), auth, TransportModeH1, settings2)
		defer t2.Close()

		deadline := time.Now().Add(5 * time.Second)
		for acceptCount2.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if acceptCount2.Load() < 2 {
			t.Fatalf("expected reconnect for reason 2, got count %d", acceptCount2.Load())
		}
		if backoff2.Status().Exceeded {
			t.Fatal("expected no hold for reason 2")
		}
		select {
		case <-parked2:
			t.Fatal("unexpected park on hold for reason 2")
		default:
		}
	}()

	// Close 1000: no hold
	func() {
		ctx3, cancel3 := context.WithCancel(context.Background())
		defer cancel3()
		backoff3 := newTestingClientLimitBackoff(newTestingClientLimitClock(), 0)
		parked3 := make(chan struct{}, 16)
		var acceptCount3 atomic.Int64

		server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := acceptCount3.Add(1)
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			if count == 1 {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "normal"), time.Now().Add(time.Second))
				return
			}
		}))
		defer server3.Close()

		settings3 := DefaultPlatformTransportSettings()
		settings3.ReconnectTimeout = 5 * time.Millisecond
		settings3.ClientLimitBackoff = backoff3
		settings3.clientLimitHoldForTest = func() {
			select {
			case parked3 <- struct{}{}:
			default:
			}
		}

		strategy3 := NewClientStrategyWithDefaults(ctx3)
		rm3 := NewRouteManager(ctx3, "test-h1-close1000")
		t3 := NewPlatformTransportWithTargetMode(ctx3, strategy3, rm3, "ws"+strings.TrimPrefix(server3.URL, "http"), auth, TransportModeH1, settings3)
		defer t3.Close()

		deadline := time.Now().Add(5 * time.Second)
		for acceptCount3.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if acceptCount3.Load() < 2 {
			t.Fatalf("expected reconnect for close 1000, got count %d", acceptCount3.Load())
		}
		if backoff3.Status().Exceeded {
			t.Fatal("expected no hold for close 1000")
		}
		select {
		case <-parked3:
			t.Fatal("unexpected park on hold for close 1000")
		default:
		}
	}()
}

// 10. TestPlatformTransportH1CloseCode4001HoldsDials
func TestPlatformTransportH1CloseCode4001HoldsDials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	parked := make(chan struct{}, 16)
	var acceptCount atomic.Int64

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := acceptCount.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		if count == 1 {
			// send websocket close code 4001
			_ = ws.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(ClientLimitCloseCode, ClientLimitCloseText),
				time.Now().Add(time.Second),
			)
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 5 * time.Millisecond
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}

	strategy := NewClientStrategyWithDefaults(ctx)
	rm := NewRouteManager(ctx, "test-h1-code4001")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}

	transport := NewPlatformTransportWithTargetMode(ctx, strategy, rm, "ws"+strings.TrimPrefix(server.URL, "http"), auth, TransportModeH1, settings)
	defer transport.Close()

	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("transport did not park on client limit hold after 4001 close code")
	}

	if count := acceptCount.Load(); count != 1 {
		t.Fatalf("expected acceptCount 1, got %d", count)
	}
	if !backoff.Status().Exceeded {
		t.Fatal("expected backoff status Exceeded to be true")
	}
}

// 13. TestClientLimitCloseForSupersededIntentTakesNoHold (SetAuth flips intent mid-connection).
func TestClientLimitCloseForSupersededIntentTakesNoHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	parked := make(chan struct{}, 16)
	var acceptCount atomic.Int64
	secondIntent := make(chan string, 1)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := acceptCount.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		if count == 1 {
			time.Sleep(50 * time.Millisecond)
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{byte(TransportControlClose), 0, 0, 0, 1})
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		} else {
			select {
			case secondIntent <- r.Header.Get(HeaderProvideIntent):
			default:
			}
		}
	}))
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 5 * time.Millisecond
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}

	strategy := NewClientStrategyWithDefaults(ctx)
	rm := NewRouteManager(ctx, "test-superseded-intent")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: false,
	}

	transport := NewPlatformTransportWithTargetMode(ctx, strategy, rm, "ws"+strings.TrimPrefix(server.URL, "http"), auth, TransportModeH1, settings)
	defer transport.Close()

	deadline := time.Now().Add(5 * time.Second)
	for acceptCount.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	transport.SetAuth(&ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	})

	select {
	case intent := <-secondIntent:
		if intent != ProvideIntentDeclared {
			t.Fatalf("redial declared %q, want %q", intent, ProvideIntentDeclared)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("redial did not occur after superseded intent close")
	}

	if backoff.Status().Exceeded {
		t.Fatal("close for superseded intent took a hold")
	}
	select {
	case <-parked:
		t.Fatal("close for superseded intent parked on hold")
	default:
	}
}

// 14. Pool: close-control path returns its buffer.
func TestPlatformTransportCloseControlReturnsPoolBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	parked := make(chan struct{}, 16)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteMessage(websocket.BinaryMessage, []byte{byte(TransportControlClose), 0, 0, 0, 1})
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}

	strategy := NewClientStrategyWithDefaults(ctx)
	rm := NewRouteManager(ctx, "test-pool-return")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}

	transport := NewPlatformTransportWithTargetMode(ctx, strategy, rm, "ws"+strings.TrimPrefix(server.URL, "http"), auth, TransportModeH1, settings)
	defer transport.Close()

	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("transport did not park on client limit hold")
	}

	waitForPoolBalance(t, 2048)
}

// 11. TestPlatformTransportH3PreAuthCloseHoldsDials: local quic listener CloseWithError(4001) before echo;
// hold set; noteBackendFailure count unchanged.
func TestPlatformTransportH3PreAuthCloseHoldsDials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverTls := selfSignedTlsConfig(t)
	udpConn := listenLoopbackUDP(t)
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	listener, err := quic.Listen(udpConn, serverTls, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				stream, err := c.AcceptStream(ctx)
				if err != nil {
					return
				}
				framer := NewFramer(DefaultFramerSettings(1024))
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				defer MessagePoolReturn(authBytes)
				// CloseWithError(4001) before echo
				c.CloseWithError(ClientLimitCloseCode, ClientLimitCloseText)
			}(conn)
		}
	}()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	parked := make(chan struct{}, 16)

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 5 * time.Millisecond
	settings.H3Port = port
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "localhost",
		NextProtos:         []string{"h3count"},
	}
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}

	initFails := backendFails()

	strategy := NewClientStrategyWithDefaults(ctx)
	rm := NewRouteManager(ctx, "test-h3-preauth")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}

	transport := NewPlatformTransportWithTargetMode(ctx, strategy, rm, "https://localhost", auth, TransportModeH3, settings)
	defer transport.Close()

	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("transport did not park on client limit hold after H3 pre-auth close")
	}

	if !backoff.Status().Exceeded {
		t.Fatal("expected backoff status Exceeded to be true")
	}
	if got := backendFails(); got != initFails {
		t.Fatalf("backend fails changed after client limit close: was %d, got %d", initFails, got)
	}
}

// 12. TestPlatformTransportHoldClosesLiveH1AndH3
func TestPlatformTransportHoldClosesLiveH1AndH3(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	h1Parked := make(chan struct{}, 16)
	h3Parked := make(chan struct{}, 16)

	var h1Accept atomic.Int64
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	h1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := h1Accept.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if count == 1 {
			// keep connection alive until we trigger client limit close
			time.Sleep(100 * time.Millisecond)
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{byte(TransportControlClose), 0, 0, 0, 1})
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer h1Server.Close()

	serverTls := selfSignedTlsConfig(t)
	udpConn := listenLoopbackUDP(t)
	h3Port := udpConn.LocalAddr().(*net.UDPAddr).Port
	h3Listener, err := quic.Listen(udpConn, serverTls, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h3Listener.Close()

	h3Closed := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := h3Listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				stream, err := c.AcceptStream(ctx)
				if err != nil {
					return
				}
				framer := NewFramer(DefaultFramerSettings(1024))
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				defer MessagePoolReturn(authBytes)
				if err := framer.Write(stream, authBytes); err != nil {
					return
				}
				// Wait until connection closes
				<-c.Context().Done()
				select {
				case h3Closed <- struct{}{}:
				default:
				}
			}(conn)
		}
	}()

	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}

	// H1 transport
	h1Settings := DefaultPlatformTransportSettings()
	h1Settings.ReconnectTimeout = 5 * time.Millisecond
	h1Settings.ClientLimitBackoff = backoff
	h1Settings.clientLimitHoldForTest = func() {
		select {
		case h1Parked <- struct{}{}:
		default:
		}
	}
	h1Transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "test-h1-sharing"),
		"ws"+strings.TrimPrefix(h1Server.URL, "http"),
		auth,
		TransportModeH1,
		h1Settings,
	)
	defer h1Transport.Close()

	// H3 transport sharing the same backoff
	h3Settings := DefaultPlatformTransportSettings()
	h3Settings.ReconnectTimeout = 5 * time.Millisecond
	h3Settings.H3Port = h3Port
	h3Settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "localhost",
		NextProtos:         []string{"h3count"},
	}
	h3Settings.ClientLimitBackoff = backoff
	h3Settings.clientLimitHoldForTest = func() {
		select {
		case h3Parked <- struct{}{}:
		default:
		}
	}
	h3Transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "test-h3-sharing"),
		"https://localhost",
		auth,
		TransportModeH3,
		h3Settings,
	)
	defer h3Transport.Close()

	// Both should park on hold after H1 receives client limit close
	select {
	case <-h1Parked:
	case <-time.After(10 * time.Second):
		t.Fatal("H1 did not park on hold")
	}

	select {
	case <-h3Closed:
	case <-time.After(10 * time.Second):
		t.Fatal("H3 live connection was not closed by hold")
	}

	select {
	case <-h3Parked:
	case <-time.After(10 * time.Second):
		t.Fatal("H3 did not park on hold")
	}

	if !backoff.Status().Exceeded {
		t.Fatal("expected backoff status Exceeded to be true")
	}
}
