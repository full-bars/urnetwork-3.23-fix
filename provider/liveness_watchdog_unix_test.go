//go:build !windows

package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// The datagram systemd expects, over a real unixgram socket.
func TestNotifySystemdWatchdogSendsWatchdogOne(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", sock)

	if err := notifySystemdWatchdog(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := listener.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "WATCHDOG=1\n" {
		t.Fatalf("systemd expects WATCHDOG=1, got %q", got)
	}
}

// Without a notify socket the ping is a quiet no-op, as for every sd_notify caller.
func TestNotifySystemdWatchdogWithoutSocketIsQuiet(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifySystemdWatchdog(); err != nil {
		t.Fatalf("no socket must be a no-op, got %v", err)
	}
}
