package connect

import (
	"context"
	"testing"
	"time"
)

// switchH3Gate sets the runtime gate for one test and restores it after. The
// gate is process wide, so a test that leaves it on would make every later
// eligible transport dial.
func switchH3Gate(t *testing.T, enabled bool) {
	t.Helper()
	previous := SetH3Enabled(enabled)
	t.Cleanup(func() { SetH3Enabled(previous) })
}

func TestH3IsOffByDefault(t *testing.T) {
	if H3Enabled() {
		t.Fatal("the runtime H3 gate must default to off")
	}
	if DefaultPlatformTransportSettings().EnableH3 {
		t.Fatal("EnableH3 must default to false: runH3 opens a host UDP socket")
	}
}

func TestH3AuxiliaryOnlyInAutoModeWithEnableH3(t *testing.T) {
	switchH3Gate(t, true)
	cases := []struct {
		mode   TransportMode
		enable bool
		want   bool
	}{
		{TransportModeAuto, true, true},
		{TransportModeAuto, false, false},
		{TransportModeH3, true, false}, // the sole transport keeps the full accounting
		{TransportModeH3, false, false},
		{TransportModeH1, true, false},
	}
	for _, c := range cases {
		transport := &PlatformTransport{
			targetMode: c.mode,
			settings:   &PlatformTransportSettings{EnableH3: c.enable},
		}
		if got := transport.h3Auxiliary(); got != c.want {
			t.Fatalf("mode=%q enable=%v: h3Auxiliary=%v, want %v", c.mode, c.enable, got, c.want)
		}
	}
}

func TestNextH3BackoffDoublesToTheCap(t *testing.T) {
	base := 5 * time.Second
	got := time.Duration(0)
	var seen []time.Duration
	for i := 0; i < 12; i++ {
		got = nextH3Backoff(got, base, h3AuxiliaryMaxBackoff)
		seen = append(seen, got)
	}
	if seen[0] != base || seen[1] != 2*base || seen[2] != 4*base {
		t.Fatalf("backoff does not double from the base: %v", seen[:3])
	}
	if seen[len(seen)-1] != h3AuxiliaryMaxBackoff {
		t.Fatalf("backoff %v does not reach the cap %v", seen[len(seen)-1], h3AuxiliaryMaxBackoff)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] || seen[i] > h3AuxiliaryMaxBackoff {
			t.Fatalf("backoff must be non-decreasing and capped: %v", seen)
		}
	}
}

// proxyFailureTotal is every recorded failure for a proxy index, auth and
// timeout alike (a dial that never answers is classed a timeout).
func proxyFailureTotal(index int) int64 {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		return -1
	}
	return h.failures.AuthFailures.Load() + h.failures.TimeoutFailures.Load()
}

// newClosedPortTransport builds a transport whose H3 endpoint is a closed
// loopback UDP port, so every connect attempt fails quickly.
func newClosedPortTransport(t *testing.T, mode TransportMode, enableH3 bool) (*PlatformTransport, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if enableH3 {
		switchH3Gate(t, true)
	}
	settings := DefaultPlatformTransportSettings()
	settings.EnableH3 = enableH3
	settings.H3Port = 1
	settings.QuicConnectTimeout = 100 * time.Millisecond
	settings.QuicHandshakeTimeout = 100 * time.Millisecond
	settings.AuthTimeout = 100 * time.Millisecond
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := &PlatformTransport{
		ctx:                  ctx,
		cancel:               cancel,
		log:                  loggerOrDefault(nil),
		clientStrategy:       &ClientStrategy{settings: DefaultClientStrategySettings()},
		platformUrl:          "wss://127.0.0.1:1",
		settings:             settings,
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		targetMode:           mode,
		mode:                 NewMonitorValue[TransportMode](TransportModeNone),
	}
	transport.auth.Store(&ClientAuth{})
	return transport, cancel
}

// An auxiliary H3 that cannot connect must NOT count as a backend failure or a
// proxy auth failure: H1 is the health signal, and a filtered UDP path would
// otherwise trip the shared gate and mark the (direct) identity unhealthy.
func TestAuxiliaryH3FailureIsNotABackendOrProxyFailure(t *testing.T) {
	resetBackendDegraded()
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	t.Cleanup(resetBackendDegraded)
	RegisterProxy(0, "direct", "direct")

	transport, cancel := newClosedPortTransport(t, TransportModeAuto, true)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if got := backendFails(); got != 0 {
		t.Fatalf("auxiliary H3 failures were counted as %d backend failures", got)
	}
	if got := proxyFailureTotal(0); got != 0 {
		t.Fatalf("auxiliary H3 failures were recorded as %d proxy failures", got)
	}
}

// The same attempts as the SOLE transport do count, which proves the test above
// would catch a regression.
func TestSoleH3FailureStillCountsAsABackendAndProxyFailure(t *testing.T) {
	resetBackendDegraded()
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	t.Cleanup(resetBackendDegraded)
	RegisterProxy(0, "direct", "direct")

	transport, cancel := newClosedPortTransport(t, TransportModeH3, false)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done

	if backendFails() == 0 {
		t.Fatal("sole-transport H3 failures must still count as backend failures")
	}
	if proxyFailureTotal(0) <= 0 {
		t.Fatal("sole-transport H3 failures must still be recorded against the proxy")
	}
}

// An auxiliary H3 that authenticates must not clear the failures H1 recorded,
// or a healthy H3 would hide an H1 outage. The sole H3 (and H1) still clear.
func TestAuxiliaryH3AuthSuccessDoesNotClearBackendFailures(t *testing.T) {
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)

	for _, c := range []struct {
		name   string
		mode   TransportMode
		h3     bool
		gateOn bool
		clear  bool
	}{
		{"auxiliary h3 gate on", TransportModeAuto, true, true, false},
		{"auxiliary h3 gate off", TransportModeAuto, true, false, false},
		{"h1 in auto without h3", TransportModeAuto, false, true, true},
		{"sole h3 gate on", TransportModeH3, false, true, true},
		{"sole h3 gate off", TransportModeH3, false, false, true},
	} {
		switchH3Gate(t, c.gateOn)
		resetBackendDegraded()
		noteBackendFailure()
		noteBackendFailure()
		transport := &PlatformTransport{targetMode: c.mode, settings: &PlatformTransportSettings{EnableH3: c.h3}}
		transport.noteAuthSuccess()
		if cleared := backendFails() == 0; cleared != c.clear {
			t.Fatalf("%s: failures cleared=%v, want %v", c.name, cleared, c.clear)
		}
	}
}

// With an auxiliary H3 neither transport may idle-drain because the other won
// the election, so both stay connected. Otherwise the sole-mode behavior holds:
// a transport that is not the active mode drains.
func TestDrainsWhenInactiveExemptsBothTransportsWhileAuxiliary(t *testing.T) {
	switchH3Gate(t, true)
	aux := &PlatformTransport{targetMode: TransportModeAuto, settings: &PlatformTransportSettings{EnableH3: true}}
	auto := &PlatformTransport{targetMode: TransportModeAuto, settings: &PlatformTransportSettings{}}

	for _, c := range []struct {
		name   string
		t      *PlatformTransport
		active TransportMode
		pt     TransportMode
		want   bool
	}{
		{"aux: h3 active, h1 idle must stay", aux, TransportModeH3, TransportModeH1, false},
		{"aux: h1 active, h3 idle must stay", aux, TransportModeH1, TransportModeH3, false},
		{"aux: active mode itself", aux, TransportModeH1, TransportModeH1, false},
		{"no aux: inactive mode drains", auto, TransportModeH1, TransportModeH3Dns, true},
		{"no aux: active mode does not drain", auto, TransportModeH1, TransportModeH1, false},
	} {
		if got := c.t.drainsWhenInactive(c.active, c.pt); got != c.want {
			t.Fatalf("%s: drainsWhenInactive=%v, want %v", c.name, got, c.want)
		}
	}
}

// Eligible is not running: with the gate off an eligible identity is not an
// auxiliary H3, so H1 keeps exactly the behaviour it has without H3.
func TestEligibleIdentityWithGateOffIsNotAuxiliary(t *testing.T) {
	switchH3Gate(t, false)
	transport := &PlatformTransport{targetMode: TransportModeAuto, settings: &PlatformTransportSettings{EnableH3: true}}
	if transport.h3Auxiliary() {
		t.Fatal("gate off: must not be auxiliary")
	}
	if !transport.h3Gated() {
		t.Fatal("an eligible Auto-mode identity is gated")
	}
	if !transport.drainsWhenInactive(TransportModeH1, TransportModeH3Dns) {
		t.Fatal("gate off: an inactive mode must drain as it does without H3")
	}
	SetH3Enabled(true)
	if !transport.h3Auxiliary() {
		t.Fatal("gate on: eligible identity is auxiliary")
	}
}

func TestSetH3EnabledReturnsPreviousAndWakesWaiters(t *testing.T) {
	switchH3Gate(t, false)
	if SetH3Enabled(true) {
		t.Fatal("previous should be false")
	}
	if !SetH3Enabled(true) {
		t.Fatal("previous should be true")
	}
	_, notify := h3GateWatch()
	select {
	case <-notify:
		t.Fatal("a no-op set must not wake waiters")
	default:
	}
	SetH3Enabled(false)
	select {
	case <-notify:
	default:
		t.Fatal("a change must wake waiters")
	}
}

// With the gate off an eligible identity must open no socket and dial nothing,
// and turning the gate on starts dialing without a restart.
func TestGateOffDoesNotDialAndGateOnStartsDialing(t *testing.T) {
	switchH3Gate(t, false)
	transport, cancel := newClosedPortTransport(t, TransportModeAuto, false)
	transport.settings.EnableH3 = true // eligible, gate stays off
	done := make(chan struct{})
	before := TransportModeStats().H3Attempts
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()

	time.Sleep(400 * time.Millisecond)
	if got := TransportModeStats().H3Attempts; got != before {
		t.Fatalf("gate off: %d dial attempts, want 0", got-before)
	}

	SetH3Enabled(true)
	deadline := time.Now().Add(3 * time.Second)
	for TransportModeStats().H3Attempts == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if TransportModeStats().H3Attempts == before {
		t.Fatal("gate on: expected the transport to start dialing without a restart")
	}

	SetH3Enabled(false)
	time.Sleep(300 * time.Millisecond)
	settled := TransportModeStats().H3Attempts
	time.Sleep(500 * time.Millisecond)
	if got := TransportModeStats().H3Attempts; got != settled {
		t.Fatalf("gate turned off again: still dialing (%d more attempts)", got-settled)
	}
	cancel()
	<-done
}

// With the gate open when a dial starts but turned off before the failure is
// recorded, an auxiliary H3 connect failure must not call noteBackendFailure or
// RecordProxyAuthFailure: the transport is auxiliary regardless of whether the
// operator switch was flipped while the dial was in flight.
func TestAuxiliaryH3FailureWithSwitchOffDoesNotRecordFailure(t *testing.T) {
	resetBackendDegraded()
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	t.Cleanup(resetBackendDegraded)
	RegisterProxy(0, "direct", "direct")

	transport, cancel := newClosedPortTransport(t, TransportModeAuto, true)
	// Give a comfortable handshake timeout so the test can flip the switch
	// while the dial is in flight.
	transport.settings.QuicConnectTimeout = 150 * time.Millisecond
	transport.settings.QuicHandshakeTimeout = 150 * time.Millisecond
	transport.settings.ReconnectTimeout = 500 * time.Millisecond
	done := make(chan struct{})
	beforeAttempts := TransportModeStats().H3Attempts
	beforeFailures := TransportModeStats().H3ConnectFailures

	go func() {
		defer close(done)
		transport.runH3(TransportModeH3, 0, 1)
	}()

	// Wait for the dial attempt to start.
	deadline := time.Now().Add(3 * time.Second)
	for TransportModeStats().H3Attempts == beforeAttempts && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if TransportModeStats().H3Attempts == beforeAttempts {
		cancel()
		<-done
		t.Fatal("timed out waiting for H3 dial attempt to start")
	}

	// Turn the switch off while the dial is in flight.
	SetH3Enabled(false)

	// Wait for the in-flight dial to fail.
	deadline = time.Now().Add(3 * time.Second)
	for TransportModeStats().H3ConnectFailures == beforeFailures && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if TransportModeStats().H3ConnectFailures == beforeFailures {
		cancel()
		<-done
		t.Fatal("timed out waiting for H3 dial failure to be recorded")
	}

	cancel()
	<-done

	if got := backendFails(); got != 0 {
		t.Fatalf("auxiliary H3 connect failure with switch turned off was counted as %d backend failures", got)
	}
	if got := proxyFailureTotal(0); got != 0 {
		t.Fatalf("auxiliary H3 connect failure with switch turned off was recorded as %d proxy failures", got)
	}
}

// Explicit H3 target mode is the sole transport and keeps full accounting
// even when the runtime switch is off.
func TestExplicitH3TargetModeStillRecordsFailures(t *testing.T) {
	switchH3Gate(t, false)
	resetBackendDegraded()
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	t.Cleanup(resetBackendDegraded)
	RegisterProxy(0, "direct", "direct")

	transport, cancel := newClosedPortTransport(t, TransportModeH3, false)
	transport.settings.QuicConnectTimeout = 100 * time.Millisecond
	transport.settings.QuicHandshakeTimeout = 100 * time.Millisecond
	transport.settings.ReconnectTimeout = 20 * time.Millisecond
	done := make(chan struct{})
	beforeFailures := TransportModeStats().H3ConnectFailures

	go func() {
		defer close(done)
		transport.runH3(TransportModeH3, 0, 1)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for TransportModeStats().H3ConnectFailures == beforeFailures && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if TransportModeStats().H3ConnectFailures == beforeFailures {
		cancel()
		<-done
		t.Fatal("timed out waiting for explicit H3 dial failure")
	}

	cancel()
	<-done

	if backendFails() == 0 {
		t.Fatal("explicit H3 target mode failures must count as backend failures")
	}
	if proxyFailureTotal(0) <= 0 {
		t.Fatal("explicit H3 target mode failures must be recorded against the proxy")
	}
}

// A global Pulse must not reset the backoff timer of an auxiliary H3 transport,
// preventing a thundering herd across proxies.
func TestPulseDoesNotResetAuxiliaryH3Backoff(t *testing.T) {
	switchH3Gate(t, true)
	transport, cancel := newClosedPortTransport(t, TransportModeAuto, true)
	transport.settings.QuicConnectTimeout = 50 * time.Millisecond
	transport.settings.QuicHandshakeTimeout = 50 * time.Millisecond
	transport.settings.ReconnectTimeout = 1 * time.Second
	done := make(chan struct{})
	beforeFailures := TransportModeStats().H3ConnectFailures

	go func() {
		defer close(done)
		transport.runH3(TransportModeH3, 0, 1)
	}()

	// Wait for the first attempt to fail and enter backoff.
	deadline := time.Now().Add(3 * time.Second)
	for TransportModeStats().H3ConnectFailures == beforeFailures && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if TransportModeStats().H3ConnectFailures == beforeFailures {
		cancel()
		<-done
		t.Fatal("timed out waiting for first H3 connect failure")
	}

	attemptsAfterFirstFail := TransportModeStats().H3Attempts

	// Trigger a global Pulse while in backoff.
	TriggerPulse()

	// Wait a short duration: auxiliary H3 must not wake up or retry immediately.
	time.Sleep(150 * time.Millisecond)
	if got := TransportModeStats().H3Attempts; got != attemptsAfterFirstFail {
		cancel()
		<-done
		t.Fatalf("Pulse triggered premature retry on auxiliary H3: attempts=%d, want %d", got, attemptsAfterFirstFail)
	}

	cancel()
	<-done
}

// A global Pulse must still wake and reset the backoff for non-auxiliary transports.
func TestPulseResetsNonAuxiliaryH3Backoff(t *testing.T) {
	switchH3Gate(t, true)
	transport, cancel := newClosedPortTransport(t, TransportModeH3, false)
	transport.settings.QuicConnectTimeout = 50 * time.Millisecond
	transport.settings.QuicHandshakeTimeout = 50 * time.Millisecond
	// Long backoff so it won't retry on its own during the test.
	transport.settings.ReconnectTimeout = 10 * time.Second
	done := make(chan struct{})
	beforeFailures := TransportModeStats().H3ConnectFailures

	go func() {
		defer close(done)
		transport.runH3(TransportModeH3, 0, 1)
	}()

	// Wait for the first attempt to fail and enter backoff.
	deadline := time.Now().Add(3 * time.Second)
	for TransportModeStats().H3ConnectFailures == beforeFailures && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if TransportModeStats().H3ConnectFailures == beforeFailures {
		cancel()
		<-done
		t.Fatal("timed out waiting for first H3 connect failure")
	}

	attemptsAfterFirstFail := TransportModeStats().H3Attempts

	// Trigger a global Pulse while in backoff: non-auxiliary H3 must wake and retry.
	TriggerPulse()

	deadline = time.Now().Add(2 * time.Second)
	for TransportModeStats().H3Attempts == attemptsAfterFirstFail && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if TransportModeStats().H3Attempts == attemptsAfterFirstFail {
		cancel()
		<-done
		t.Fatal("expected Pulse to wake non-auxiliary H3 and trigger retry")
	}

	cancel()
	<-done
}

