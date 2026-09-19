//go:build linux

package main

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// stubMetricsAuto points auto mode at one free port, no container marker
// and the given Tailscale addresses, restoring everything afterwards.
func stubMetricsAuto(t *testing.T, tailscale func() []netip.Addr) int {
	t.Helper()
	unsetMetricsEnv(t)
	_, portStr, _ := net.SplitHostPort(freeLoopbackAddr(t))
	port, _ := strconv.Atoi(portStr)

	origPorts, origMarkers, origTS, origRescan := metricsAutoPorts, metricsContainerMarkers, tailscaleAddrsFunc, tailscaleRescanInterval
	metricsAutoPorts = []int{port}
	metricsContainerMarkers = []string{filepath.Join(t.TempDir(), "no-such-marker")}
	tailscaleAddrsFunc = tailscale
	tailscaleRescanInterval = time.Hour
	t.Cleanup(func() {
		metricsAutoPorts, metricsContainerMarkers, tailscaleAddrsFunc, tailscaleRescanInterval = origPorts, origMarkers, origTS, origRescan
	})
	return port
}

func noTailscale() []netip.Addr { return nil }

// skipWithoutLoopbackAlias skips where only 127.0.0.1 is bindable (macOS
// without an alias), since the tests stand 127.0.0.x in for a Tailscale IP.
func skipWithoutLoopbackAlias(t *testing.T, ip string) {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("%s not bindable here: %v", ip, err)
	}
	ln.Close()
}

func TestValidateMetricsListen(t *testing.T) {
	good := []string{"auto", "AUTO", "off", "127.0.0.1:9100", "0.0.0.0:9100", "100.64.0.10:9101", "[::1]:9100", "[::]:9100"}
	bad := []string{"", "on", "9100", ":9100", "localhost:9100", "tailscale", "1.2.3.4", "1.2.3.4:0", "1.2.3.4:70000", "1.2.3.4:http"}
	for _, v := range good {
		if err := validateMetricsListen(v); err != nil {
			t.Errorf("validateMetricsListen(%q) = %v, want nil", v, err)
		}
		if err := validateControlValue("metrics_listen", v); err != nil {
			t.Errorf("validateControlValue(metrics_listen, %q) = %v, want nil", v, err)
		}
	}
	for _, v := range bad {
		if validateMetricsListen(v) == nil {
			t.Errorf("validateMetricsListen(%q) accepted a bad value", v)
		}
	}
}

// TestListenMetricsAutoServesLoopbackAndTailscale: with no explicit address,
// /metrics listens on loopback and on the Tailscale address, same port, and
// a connection to either reaches the one listener.
func TestListenMetricsAutoServesLoopbackAndTailscale(t *testing.T) {
	skipWithoutLoopbackAlias(t, "127.0.0.2")
	ts := netip.MustParseAddr("127.0.0.2")
	port := stubMetricsAuto(t, func() []netip.Addr { return []netip.Addr{ts} })

	ln, err := listenMetrics(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	want := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), net.JoinHostPort("127.0.0.2", strconv.Itoa(port))}
	multi, ok := ln.(*metricsMultiListener)
	if !ok {
		t.Fatalf("listener is %T, want *metricsMultiListener", ln)
	}
	if got := multi.Addrs(); !slices.Equal(got, want) {
		t.Fatalf("Addrs() = %v, want %v", got, want)
	}

	for _, addr := range want {
		go func() {
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				c.Close()
			}
		}()
		conn, err := acceptWithin(ln, 2*time.Second)
		if err != nil {
			t.Fatalf("no connection accepted from %s: %v", addr, err)
		}
		conn.Close()
	}
}

func acceptWithin(ln net.Listener, d time.Duration) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(d):
		return nil, errors.New("timed out")
	}
}

// TestListenMetricsPicksUpLateTailscale: Tailscale often comes up after the
// provider at boot. The rescan must add its address without a restart.
func TestListenMetricsPicksUpLateTailscale(t *testing.T) {
	skipWithoutLoopbackAlias(t, "127.0.0.3")
	var up atomic.Bool
	ts := netip.MustParseAddr("127.0.0.3")
	port := stubMetricsAuto(t, func() []netip.Addr {
		if up.Load() {
			return []netip.Addr{ts}
		}
		return nil
	})
	tailscaleRescanInterval = 20 * time.Millisecond

	ln, err := listenMetrics(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	multi := ln.(*metricsMultiListener)
	if got := multi.Addrs(); len(got) != 1 {
		t.Fatalf("Addrs() before Tailscale = %v, want loopback only", got)
	}

	up.Store(true)
	want := net.JoinHostPort("127.0.0.3", strconv.Itoa(port))
	deadline := time.Now().Add(3 * time.Second)
	for !slices.Contains(multi.Addrs(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("Addrs() = %v, never picked up %s", multi.Addrs(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestListenMetricsInContainerBindsAllInterfaces: loopback inside a
// container is unreachable from the host even with -p.
func TestListenMetricsInContainerBindsAllInterfaces(t *testing.T) {
	port := stubMetricsAuto(t, noTailscale)
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	metricsContainerMarkers = []string{marker}

	ln, err := listenMetrics(0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if !ap.Addr().IsUnspecified() || int(ap.Port()) != port {
		t.Fatalf("container listener on %s, want 0.0.0.0:%d", ln.Addr(), port)
	}
}

// TestMetricsListenSettingRebindsLive: `metrics listen <addr>` moves a
// running listener to that address, and clearing it returns to auto, both
// without a restart, and status reports where it is listening.
func TestMetricsListenSettingRebindsLive(t *testing.T) {
	withTempHome(t)
	port := stubMetricsAuto(t, noTailscale)
	t.Cleanup(func() {
		handleControlRequest(globalControlState, controlRequest{Cmd: "clear", Key: "metrics_listen"})
		_ = applyMetricsLive("off")
		ClearCoordinatorClosers()
	})

	if err := applyMetricsLive("on"); err != nil {
		t.Fatal(err)
	}
	auto := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if got := handleControlRequest(globalControlState, controlRequest{Cmd: "status"}).MetricsAddrs; !slices.Equal(got, []string{auto}) {
		t.Fatalf("status MetricsAddrs = %v, want [%s]", got, auto)
	}

	explicit := freeLoopbackAddr(t)
	resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "metrics_listen", Value: explicit})
	if !resp.OK {
		t.Fatalf("set metrics_listen: %s", resp.Error)
	}
	if resp.NeedsRestart {
		t.Error("set metrics_listen reported a restart is needed")
	}
	if got := metricsServedAddrs(); !slices.Equal(got, []string{explicit}) {
		t.Fatalf("after set, serving %v, want [%s]", got, explicit)
	}
	if c, err := net.DialTimeout("tcp", auto, 200*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("old auto address %s still accepts connections", auto)
	}

	resp = handleControlRequest(globalControlState, controlRequest{Cmd: "clear", Key: "metrics_listen"})
	if !resp.OK {
		t.Fatalf("clear metrics_listen: %s", resp.Error)
	}
	if got := metricsServedAddrs(); !slices.Equal(got, []string{auto}) {
		t.Fatalf("after clear, serving %v, want [%s]", got, auto)
	}
}

// TestMetricsListenIgnoredWhileOff: setting an address with metrics off
// must not start a listener.
func TestMetricsListenIgnoredWhileOff(t *testing.T) {
	withTempHome(t)
	stubMetricsAuto(t, noTailscale)
	_ = applyMetricsLive("off")
	t.Cleanup(func() {
		handleControlRequest(globalControlState, controlRequest{Cmd: "clear", Key: "metrics_listen"})
	})
	resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "metrics_listen", Value: freeLoopbackAddr(t)})
	if !resp.OK {
		t.Fatalf("set metrics_listen: %s", resp.Error)
	}
	if metricsServer != nil || metricsServedAddrs() != nil {
		t.Fatalf("metrics_listen started a listener while metrics is off: %v", metricsServedAddrs())
	}
}

func TestMetricsMultiListenerClose(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := newMetricsMultiListener(0, first)
	if err := m.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := m.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Close = %v, want net.ErrClosed", err)
	}
	if _, err := m.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
	if c, err := net.DialTimeout("tcp", first.Addr().String(), 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("underlying listener still accepts after Close")
	}
	// A listener added after Close is closed rather than leaked.
	late, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.add(late)
	if c, err := net.DialTimeout("tcp", late.Addr().String(), 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("listener added after Close was left open")
	}
}

func TestIsTailscaleInterface(t *testing.T) {
	for _, name := range []string{"tailscale0", "Tailscale", "tailscale1"} {
		if !isTailscaleInterface(name) {
			t.Errorf("isTailscaleInterface(%q) = false", name)
		}
	}
	for _, name := range []string{"eth0", "wg0", "docker0", "lo"} {
		if isTailscaleInterface(name) {
			t.Errorf("isTailscaleInterface(%q) = true", name)
		}
	}
}
