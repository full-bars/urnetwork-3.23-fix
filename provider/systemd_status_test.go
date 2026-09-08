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
	t.Cleanup(func() {
		proxiesConfigured.Store(0)
		proxiesAuthenticated.Store(0)
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
	cases := []struct {
		name        string
		total, live int64
		want        string
	}{
		{"before proxies resolve", 0, 0, "starting: resolving proxies"},
		{"none authenticated", 3, 0, "degraded: 0/3 proxies authenticated, retrying"},
		{"some authenticated", 3, 2, "partial: 2/3 proxies authenticated"},
		{"all authenticated", 3, 3, "active: 3/3 proxies authenticated"},
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
	if got := systemdStatusLine(); got != "partial: 1/2 proxies authenticated" {
		t.Errorf("after clamp then live, got %q", got)
	}
}
