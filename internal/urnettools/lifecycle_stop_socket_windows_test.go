//go:build windows

package urnettools

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A force stop of one process must not delete the control socket a live
// provider still listens on (two processes can share the default state dir
// on Windows, and discovery shows only one of them).
func TestCleanupStaleSocketKeepsLiveListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "provider.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	cleanupStaleSocket(sock)

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("live provider no longer reachable after cleanup: %v", err)
	}
	c.Close()
}

func TestCleanupStaleSocketRemovesDeadSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "provider.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupStaleSocket(sock)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("dead socket file left behind: %v", err)
	}
}
