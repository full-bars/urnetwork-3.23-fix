package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type ipv6StageDials struct {
	dials       proxyIPv6Dials
	connectRuns atomic.Int32
	controlRuns atomic.Int32
	tcpExitRuns atomic.Int32
	udpExitRuns atomic.Int32
}

// newIPv6StageDials builds fake stage dials. connect answers with the given
// reply code or error, control with its error (nil means the IPv4 control
// passed), and the exits with the given addresses or errors.
func newIPv6StageDials(code byte, connectErr error, controlErr error, tcpExit string, tcpErr error, udpExit string, udpErr error) *ipv6StageDials {
	d := &ipv6StageDials{}
	d.dials = proxyIPv6Dials{
		connect: func(context.Context, *connect.ProxySettings, string) (byte, error) {
			d.connectRuns.Add(1)
			return code, connectErr
		},
		control: func(context.Context, *connect.ProxySettings, string) (byte, error) {
			d.controlRuns.Add(1)
			return 0, controlErr
		},
		tcpExit: func(context.Context, *connect.ProxySettings) (string, error) {
			d.tcpExitRuns.Add(1)
			return tcpExit, tcpErr
		},
		udpExit: func(context.Context, *connect.ProxySettings) (string, error) {
			d.udpExitRuns.Add(1)
			return udpExit, udpErr
		},
	}
	return d
}

func TestRunProxyIPv6Stage(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	timeout := proxyIPv6ConnectTimeout{errors.New("connect reply timeout")}
	other := errors.New("proxy refused")

	tests := []struct {
		name         string
		dials        *ipv6StageDials
		wantStatus   proxyIPv6Status
		wantUDP      string
		wantInterval time.Duration
		wantTCPExit  string
		wantUDPExit  string
		wantControl  bool
		wantStage23  bool
	}{
		{
			name:         "ipv6 reachable and the udp exit matches",
			dials:        newIPv6StageDials(0, nil, nil, "2001:db8::1", nil, "2001:db8::1", nil),
			wantStatus:   proxyIPv6Yes,
			wantUDP:      "yes",
			wantInterval: 12 * time.Hour,
			wantTCPExit:  "2001:db8::1",
			wantUDPExit:  "2001:db8::1",
			wantStage23:  true,
		},
		{
			name:         "connection refused by the target still proves ipv6",
			dials:        newIPv6StageDials(5, nil, nil, "2001:db8::1", nil, "2001:db8::1", nil),
			wantStatus:   proxyIPv6Yes,
			wantUDP:      "yes",
			wantInterval: 12 * time.Hour,
			wantTCPExit:  "2001:db8::1",
			wantUDPExit:  "2001:db8::1",
			wantStage23:  true,
		},
		{
			name:         "udp exit differs from the tcp exit",
			dials:        newIPv6StageDials(0, nil, nil, "2001:db8::1", nil, "2001:db8::2", nil),
			wantStatus:   proxyIPv6Yes,
			wantUDP:      "mismatch",
			wantInterval: 12 * time.Hour,
			wantTCPExit:  "2001:db8::1",
			wantUDPExit:  "2001:db8::2",
			wantStage23:  true,
		},
		{
			name:         "tcp ipv6 works but the tcp exit lookup fails",
			dials:        newIPv6StageDials(0, nil, nil, "", other, "2001:db8::1", nil),
			wantStatus:   proxyIPv6Yes,
			wantUDP:      "unknown",
			wantInterval: time.Hour, // the UDP exit is not known: retry soon
			wantStage23:  true,
		},
		{
			name:         "tcp ipv6 works but the udp stun lookup fails",
			dials:        newIPv6StageDials(0, nil, nil, "2001:db8::1", nil, "", other),
			wantStatus:   proxyIPv6Yes,
			wantUDP:      "unknown",
			wantInterval: time.Hour, // the UDP exit is not known: retry soon
			wantTCPExit:  "2001:db8::1",
			wantStage23:  true,
		},
		{
			name:         "network unreachable reply means no, with no later stages",
			dials:        newIPv6StageDials(3, nil, nil, "2001:db8::1", nil, "2001:db8::1", nil),
			wantStatus:   proxyIPv6No,
			wantUDP:      "unknown",
			wantInterval: 24 * time.Hour,
		},
		{
			name:         "ruleset refusal says nothing",
			dials:        newIPv6StageDials(2, nil, nil, "", nil, "", nil),
			wantStatus:   proxyIPv6Unknown,
			wantUDP:      "unknown",
			wantInterval: time.Hour,
			wantControl:  true,
		},
		{
			name:         "timeout with a working ipv4 control means no",
			dials:        newIPv6StageDials(0, timeout, nil, "", nil, "", nil),
			wantStatus:   proxyIPv6No,
			wantUDP:      "unknown",
			wantInterval: 24 * time.Hour,
			wantControl:  true,
		},
		{
			name:         "timeout with a failing ipv4 control stays unknown",
			dials:        newIPv6StageDials(0, timeout, other, "", nil, "", nil),
			wantStatus:   proxyIPv6Unknown,
			wantUDP:      "unknown",
			wantInterval: time.Hour,
			wantControl:  true,
		},
		{
			name:         "a refused connection to the proxy stays unknown",
			dials:        newIPv6StageDials(0, other, nil, "", nil, "", nil),
			wantStatus:   proxyIPv6Unknown,
			wantUDP:      "unknown",
			wantInterval: time.Hour,
			wantControl:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, measured := runProxyIPv6Stage(context.Background(), settings, proxyIPv6Result{}, now, tt.dials.dials)
			if !measured {
				t.Fatal("the stage reported no measurement")
			}
			if result.Status != tt.wantStatus || result.UDP != tt.wantUDP || result.Interval != tt.wantInterval {
				t.Fatalf("result = %+v, want status %v udp %q interval %v", result, tt.wantStatus, tt.wantUDP, tt.wantInterval)
			}
			if result.TCPExit != tt.wantTCPExit || result.UDPExit != tt.wantUDPExit {
				t.Fatalf("exits = %q %q, want %q %q", result.TCPExit, result.UDPExit, tt.wantTCPExit, tt.wantUDPExit)
			}
			if result.Source != "measured" || !result.At.Equal(now) {
				t.Fatalf("source or time wrong: %+v", result)
			}
			if got := tt.dials.controlRuns.Load() > 0; got != tt.wantControl {
				t.Fatalf("control ran = %v, want %v", got, tt.wantControl)
			}
			if got := tt.dials.tcpExitRuns.Load() > 0; got != tt.wantStage23 {
				t.Fatalf("exit lookups ran = %v, want %v", got, tt.wantStage23)
			}
		})
	}
}

func TestRunProxyIPv6StageKeepsThePreviousResultWhenCancelled(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	previous := proxyIPv6Result{Status: proxyIPv6Yes, Source: "measured", At: now.Add(-13 * time.Hour), Interval: 12 * time.Hour}
	d := newIPv6StageDials(3, nil, nil, "", nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, measured := runProxyIPv6Stage(ctx, settings, previous, now, d.dials)
	if measured || result != previous {
		t.Fatalf("a cancelled stage returned %+v, %v", result, measured)
	}
	if d.connectRuns.Load() != 0 {
		t.Fatal("a cancelled stage still dialed")
	}
}

func TestRunProxyIPv6StageDoublesAStableInterval(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := newIPv6StageDials(3, nil, nil, "", nil, "", nil)
	first, _ := runProxyIPv6Stage(context.Background(), settings, proxyIPv6Result{}, now, d.dials)
	second, _ := runProxyIPv6Stage(context.Background(), settings, first, now.Add(first.Interval), d.dials)
	if second.Interval != 48*time.Hour {
		t.Fatalf("second no answer interval = %v, want 48h", second.Interval)
	}
	third, _ := runProxyIPv6Stage(context.Background(), settings, second, now.Add(72*time.Hour), d.dials)
	if third.Interval != 96*time.Hour {
		t.Fatalf("third no answer interval = %v, want 96h", third.Interval)
	}
	// a changed answer starts again from the base interval
	changed := newIPv6StageDials(0, nil, nil, "2001:db8::1", nil, "2001:db8::1", nil)
	back, _ := runProxyIPv6Stage(context.Background(), settings, third, now.Add(200*time.Hour), changed.dials)
	if back.Status != proxyIPv6Yes || back.Interval != 12*time.Hour {
		t.Fatalf("a changed answer gave %+v", back)
	}
}

// Only a reply that proves forwarding counts as the IPv4 control.
func TestIPv4ControlNeedsAReplyThatProvesForwarding(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	timeout := proxyIPv6ConnectTimeout{errors.New("connect reply timeout")}
	for _, test := range []struct {
		code byte
		want proxyIPv6Status
	}{
		{0, proxyIPv6No},      // forwarded: an IPv6 timeout is a real answer
		{5, proxyIPv6No},      // refused by the target: the proxy still forwarded
		{2, proxyIPv6Unknown}, // not allowed by the ruleset: it forwarded nothing
		{3, proxyIPv6Unknown}, // network unreachable
		{1, proxyIPv6Unknown}, // general failure
	} {
		d := newIPv6StageDials(0, timeout, nil, "", nil, "", nil)
		code := test.code
		d.dials.control = func(context.Context, *connect.ProxySettings, string) (byte, error) { return code, nil }
		result, _ := runProxyIPv6Stage(context.Background(), settings, proxyIPv6Result{}, now, d.dials)
		if result.Status != test.want {
			t.Fatalf("control reply %d gave %v, want %v", code, result.Status, test.want)
		}
	}
}

// A run cut short by its context keeps the previous result.
func TestRunProxyIPv6StageCancelledDuringTheFirstDialKeepsThePreviousResult(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	previous := proxyIPv6Result{Status: proxyIPv6Yes, Source: "measured", UDP: "yes", At: now.Add(-13 * time.Hour), Interval: 12 * time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	d := newIPv6StageDials(0, nil, nil, "", nil, "", nil)
	d.dials.connect = func(context.Context, *connect.ProxySettings, string) (byte, error) {
		cancel()
		return 0, errors.New("context canceled")
	}
	result, measured := runProxyIPv6Stage(ctx, settings, previous, now, d.dials)
	if measured || result != previous {
		t.Fatalf("a cancelled run returned %+v, %v", result, measured)
	}
}

func TestResolveProxyIPv6UDPAddr(t *testing.T) {
	ctx := context.Background()
	addr, err := resolveProxyIPv6UDPAddr(ctx, "[2001:db8::1]:3478")
	if err != nil || addr.Port != 3478 || addr.IP.String() != "2001:db8::1" {
		t.Fatalf("literal = %v, %v", addr, err)
	}
	for _, bad := range []string{"192.0.2.1:3478", "[2001:db8::1]", "[2001:db8::1]:0", "[2001:db8::1]:70000", "[2001:db8::1]:x"} {
		if _, err := resolveProxyIPv6UDPAddr(ctx, bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := resolveProxyIPv6UDPAddr(cancelled, "stun.invalid:3478"); err == nil {
		t.Fatal("a cancelled lookup returned an address")
	}
}
