package main

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestUDPInvariantNoLeakedFlowsOrSocks ensures that opening and closing many
// proxied udp flows leaves no goroutines or file descriptors behind.
//
// The warm-up flow runs lazy initialization before the baseline is sampled, so
// the baseline itself is not measured. Then flows are opened and closed, associations
// drain to zero, and both counts must return to baseline.
func TestUDPInvariantNoLeakedFlowsOrSocks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd count via /proc/self/fd is linux-only")
	}
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

	// warm up, then wait for the one association to close
	warm, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("warm-up dial: %v", err)
	}
	warm.Close()
	waitForAssociations(t, server, 0)

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()
	baselineFiles := countOpenFds(t)

	const flows = 20
	for i := 0; i < flows; i++ {
		conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
		if err != nil {
			t.Fatalf("flow %d dial: %v", i, err)
		}
		if _, err := conn.Write([]byte("leak")); err != nil {
			conn.Close()
			t.Fatalf("flow %d write: %v", i, err)
		}
		conn.Close()
	}
	waitForAssociations(t, server, 0)

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		files := countOpenFds(t)
		goroutines := runtime.NumGoroutine()
		if files <= baselineFiles && goroutines <= baselineGoroutines+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource leak after %d flows: goroutines %d -> %d, file descriptors %d -> %d",
				flows, baselineGoroutines, goroutines, baselineFiles, files)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countOpenFds(t *testing.T) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}
