package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// notifyRecorder stands up a real unixgram socket and points NOTIFY_SOCKET at
// it, so these tests exercise the actual sd_notify wire format rather than a
// stub. Mirrors the harness in TestNotifySystemdMainPID.
func notifyRecorder(t *testing.T) *net.UnixConn {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "notify.sock")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	t.Setenv("NOTIFY_SOCKET", sockPath)
	return l
}

func readDatagram(t *testing.T, l *net.UnixConn) string {
	t.Helper()
	if err := l.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}
	return string(buf[:n])
}

func resetProxyCounters(t *testing.T) {
	t.Helper()
	proxiesConfigured.Store(0)
	proxiesAuthenticated.Store(0)
	resetProxyResolutionStatus()
	t.Cleanup(func() {
		proxiesConfigured.Store(0)
		proxiesAuthenticated.Store(0)
		resetProxyResolutionStatus()
	})
}

// The core regression: READY must be sendable with zero proxies
// authenticated. If READY is ever re-coupled to proxy auth, this fails.
func TestNotifySystemdReadyIndependentOfProxyAuth(t *testing.T) {
	l := notifyRecorder(t)
	resetProxyCounters(t)
	proxiesConfigured.Store(5)
	proxiesAuthenticated.Store(0)

	if err := notifySystemdReady(); err != nil {
		t.Fatalf("notifySystemdReady with 0 authenticated proxies: %v", err)
	}
	if got, want := readDatagram(t, l), "READY=1\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNotifySystemdStatusWireFormat(t *testing.T) {
	l := notifyRecorder(t)
	if err := notifySystemdStatus("degraded: 0/3 proxies authenticated, retrying"); err != nil {
		t.Fatalf("notifySystemdStatus: %v", err)
	}
	want := "STATUS=degraded: 0/3 proxies authenticated, retrying\n"
	if got := readDatagram(t, l); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Absent NOTIFY_SOCKET (Type=simple, or not under systemd at all) must be a
// silent no-op, never an error: every existing fleet node is Type=simple.
func TestNotifyNoSocketIsNotAnError(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifySystemdReady(); err != nil {
		t.Errorf("notifySystemdReady without NOTIFY_SOCKET = %v, want nil", err)
	}
	if err := notifySystemdStatus("anything"); err != nil {
		t.Errorf("notifySystemdStatus without NOTIFY_SOCKET = %v, want nil", err)
	}
}

// notifySystemdMainPID must KEEP returning ErrNoNotifySocket: hotswap's F-2
// pre-flight check depends on telling "no notify socket" apart from a send
// failure, so the refactor must not smooth that away like the other two.
func TestNotifyMainPIDStillDistinguishesMissingSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifySystemdMainPID(1234); err != ErrNoNotifySocket {
		t.Errorf("notifySystemdMainPID without NOTIFY_SOCKET = %v, want ErrNoNotifySocket", err)
	}
}

func TestSystemdStatusLine(t *testing.T) {
	resetProxyCounters(t)
	resetProxyResolutionStatus()
	t.Cleanup(resetProxyResolutionStatus)
	cases := []struct {
		name        string
		total, live int64
		want        string
	}{
		{"before proxies resolve", 0, 0, "starting: resolving proxies"},
		{"none authenticated", 3, 0, "critical: 0/3 proxies authenticated, retrying"},
		{"two of three live", 3, 2, "degraded: 2/3 proxies authenticated (67%), retrying"},
		{"all authenticated", 3, 3, "active: 3/3 proxies authenticated (100%)"},
		{"90% live is active", 10, 9, "active: 9/10 proxies authenticated (90%)"},
		{"89% live is partial", 100, 89, "partial: 89/100 proxies authenticated (89%)"},
		{"70% live is partial", 10, 7, "partial: 7/10 proxies authenticated (70%)"},
		{"69% live is degraded", 100, 69, "degraded: 69/100 proxies authenticated (69%), retrying"},
		{"50% live is degraded", 10, 5, "degraded: 5/10 proxies authenticated (50%), retrying"},
		{"49% live is critical", 100, 49, "critical: 49/100 proxies authenticated (49%), retrying"},
		{"25% live is critical", 100, 25, "critical: 25/100 proxies authenticated (25%), retrying"},
		{"10% live is critical", 100, 10, "critical: 10/100 proxies authenticated (10%), retrying"},
		{"live exceeds configured clamps at 100", 2, 3, "active: 3/2 proxies authenticated (100%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxiesConfigured.Store(tc.total)
			proxiesAuthenticated.Store(tc.live)
			if got := systemdStatusLine(); got != tc.want {
				t.Errorf("systemdStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// When total==0 the status line depends on proxyResolutionStatus.
func TestSystemdStatusLineResolutionStates(t *testing.T) {
	resetProxyCounters(t)
	resetProxyResolutionStatus()
	t.Cleanup(resetProxyResolutionStatus)

	cases := []struct {
		name   string
		status int32
		reason string
		want   string
	}{
		{
			"pending resolution",
			proxyResolutionPending, "",
			"starting: resolving proxies",
		},
		{
			"source unreachable",
			proxyResolutionFailed, "connection refused",
			"degraded: proxy source unreachable (connection refused), retrying",
		},
		{
			"failed with long reason is truncated",
			proxyResolutionFailed, "a very long error message that exceeds the maximum length limit for status line display",
			"degraded: proxy source unreachable (a very long error message that exceeds the maximum length li...), retrying",
		},
		{
			"empty source",
			proxyResolutionEmpty, "",
			"degraded: proxy source returned no usable proxies, retrying",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxiesConfigured.Store(0)
			proxiesAuthenticated.Store(0)
			setProxyResolutionStatus(tc.status, tc.reason)
			if got := systemdStatusLine(); got != tc.want {
				t.Errorf("systemdStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// setConfiguredProxyCount updates the configured total and the status line
// must reflect it immediately.
func TestSystemdStatusLineReloadUpdatesTotal(t *testing.T) {
	resetProxyCounters(t)
	t.Cleanup(func() { proxiesConfigured.Store(0); proxiesAuthenticated.Store(0) })

	setConfiguredProxyCount(0)
	if got, want := systemdStatusLine(), "starting: resolving proxies"; got != want {
		t.Errorf("after setConfiguredProxyCount(0): got %q, want %q", got, want)
	}

	setConfiguredProxyCount(5)
	proxiesAuthenticated.Store(3)
	if got, want := systemdStatusLine(), "degraded: 3/5 proxies authenticated (60%), retrying"; got != want {
		t.Errorf("after setConfiguredProxyCount(5): got %q, want %q", got, want)
	}

	setConfiguredProxyCount(2)
	// Auth exceeds new configured count — line shows degraded (live < total is
	// not the case here, but live > total is nonsensical; the line still renders).
	proxiesAuthenticated.Store(3)
	if got := systemdStatusLine(); got == "" {
		t.Errorf("status line must never be empty")
	}
}

// An unbalanced proxyWentDown must clamp at zero rather than render a
// negative count in systemctl status.
func TestProxyWentDownClampsAtZero(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	resetProxyCounters(t)
	proxiesConfigured.Store(2)

	proxyWentDown()
	if got := proxiesAuthenticated.Load(); got != 0 {
		t.Errorf("authenticated count after unbalanced down = %d, want 0", got)
	}
	proxyBecameLive()
	if got := systemdStatusLine(); got != "degraded: 1/2 proxies authenticated (50%), retrying" {
		t.Errorf("after clamp then live, got %q", got)
	}
}
