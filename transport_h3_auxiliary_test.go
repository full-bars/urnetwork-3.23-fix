package connect

import (
	"context"
	"testing"
	"time"
)

func TestH3IsOffByDefault(t *testing.T) {
	if DefaultPlatformTransportSettings().EnableH3 {
		t.Fatal("EnableH3 must default to false: runH3 opens a host UDP socket")
	}
}

func TestH3AuxiliaryOnlyInAutoModeWithEnableH3(t *testing.T) {
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
		name  string
		mode  TransportMode
		h3    bool
		clear bool
	}{
		{"auxiliary h3", TransportModeAuto, true, false},
		{"h1 in auto without h3", TransportModeAuto, false, true},
		{"sole h3", TransportModeH3, false, true},
	} {
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
