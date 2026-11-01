package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Where /metrics listens. In order of precedence:
//
//  1. URNETWORK_METRICS: disable, auto, or exactly that address.
//  2. The metrics_listen setting (`urnet-tools metrics listen <ip:port>`):
//     exactly that address, persisted across restarts and updates.
//  3. "auto", the default:
//     - In a container: 0.0.0.0. Loopback inside a container cannot be
//       reached from the host even with -p, and on a bridge network only
//       the ports the operator publishes are reachable.
//     - Otherwise: 127.0.0.1 plus every Tailscale IPv4 address on the box,
//       all on the same port, so a Prometheus on the operator's tailnet can
//       scrape the node with no further setup. Never a public interface.
//       Tailscale often comes up after the provider at boot, so addresses
//       that appear later are picked up by a rescan.

// metricsAutoPorts are tried in order; the first one free on every auto
// address wins. A var so tests can use free ports.
var metricsAutoPorts = []int{9100, 9101, 9102, 9103}

// metricsContainerMarkers exist inside Docker and Podman containers.
var metricsContainerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// tailscaleAddrsFunc lists the box's Tailscale IPv4 addresses.
var tailscaleAddrsFunc = tailscaleIPv4Addrs

// tailscaleRescanInterval is how often auto mode looks for Tailscale
// addresses that appeared after /metrics started.
var tailscaleRescanInterval = 30 * time.Second

var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// tailscaleIPv4Addrs returns the IPv4 addresses Tailscale assigned to this
// machine. Only Tailscale's own interface counts: 100.64.0.0/10 is also
// carrier-grade NAT space, which some hosts carry on ordinary interfaces.
func tailscaleIPv4Addrs() []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || !isTailscaleInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.Is4() && tailscaleCGNAT.Contains(ip) {
				out = append(out, ip)
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

// isTailscaleInterface matches tailscale0 (Linux), "Tailscale" (Windows)
// and utun devices (macOS, where Tailscale's tunnel has no distinct name;
// the 100.64.0.0/10 check narrows it).
func isTailscaleInterface(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "tailscale") {
		return true
	}
	return runtime.GOOS == "darwin" && strings.HasPrefix(lower, "utun")
}

// containerAddrsFunc lists a container's own interface addresses. A var so
// tests can stub it.
var containerAddrsFunc = interfaceUnicastAddrs

func interfaceUnicastAddrs() []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if prefix, err := netip.ParsePrefix(addr.String()); err == nil {
				out = append(out, prefix.Addr().Unmap())
			}
		}
	}
	return out
}

// publiclyAddressed reports whether the box or container holds an address
// that is routable on the public internet (global unicast, not private,
// unique-local or Tailscale CGNAT). A bridged container only has a private
// address. A container that shares the host's network (--network host) sees the
// host's public addresses, and binding 0.0.0.0 there would publish /metrics
// (which carries per-proxy series) on the public internet.
func publiclyAddressed() bool {
	for _, addr := range containerAddrsFunc() {
		if addr.IsGlobalUnicast() && !addr.IsPrivate() && !tailscaleCGNAT.Contains(addr) {
			return true
		}
	}
	return false
}

func runningInContainer() bool {
	for _, marker := range metricsContainerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// metricsListenSetting returns the persisted explicit address, or "" for
// auto.
func metricsListenSetting() string {
	v, ok := globalControlState.get("metrics_listen")
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "auto") || strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

// validateMetricsListen accepts "auto" (or "off", which clears to auto) or
// an IP literal with a port. Hostnames are refused: what one resolves to
// can change between restarts, and the point is a predictable target.
func validateMetricsListen(value string) error {
	if strings.EqualFold(value, "auto") || strings.EqualFold(value, "off") {
		return nil
	}
	ap, err := netip.ParseAddrPort(value)
	if err != nil {
		return fmt.Errorf("metrics_listen: must be auto or an IP address with a port, like 100.64.0.10:9100 or 0.0.0.0:9100 (got %q)", value)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("metrics_listen: port must be 1-65535 (got %q)", value)
	}
	return nil
}

func metricsDisabledByEnv() bool {
	switch strings.ToLower(os.Getenv("URNETWORK_METRICS")) {
	case "off", "false", "no", "0", "none":
		return true
	default:
		return false
	}
}

// listenMetrics binds the /metrics listener as described at the top of this
// file. When disabled it returns (nil, nil). Otherwise the listener comes back open: probing a port, closing it and
// binding it again let another process take it in between.
//
// wait is how long to keep retrying the preferred address (the explicit
// address, or the first auto port) before giving up or moving on to the
// next port; a HotSwap candidate uses it while the parent releases its
// listener.
func listenMetrics(wait time.Duration) (net.Listener, error) {
	if metricsDisabledByEnv() {
		return nil, nil
	}
	if addr := os.Getenv("URNETWORK_METRICS"); addr != "" {
		if !strings.EqualFold(addr, "auto") {
			return listenOrWait(addr, wait)
		}
	} else if addr := metricsListenSetting(); addr != "" {
		return listenOrWait(addr, wait)
	}

	// The wide 0.0.0.0 bind is for a bridged container, where only ports the
	// operator publishes are reachable. A container that shares a publicly
	// addressed network gets the loopback + Tailscale behavior instead, so the
	// default can never publish /metrics on a public interface.
	container := runningInContainer()
	if container && publiclyAddressed() {
		tlog("[metrics] this container shares a publicly addressed network: /metrics stays on loopback and Tailscale only (use `urnet-tools metrics listen <ip:port>` to choose another address)\n")
		container = false
	}
	host := "127.0.0.1"
	if container {
		host = "0.0.0.0"
	}
	var lastErr error
	for i, port := range metricsAutoPorts {
		portWait := time.Duration(0)
		if i == 0 {
			portWait = wait
		}
		ln, err := listenOrWait(net.JoinHostPort(host, fmt.Sprint(port)), portWait)
		if err != nil {
			lastErr = err
			tlog("[metrics] port %d in use, trying next\n", port)
			continue
		}
		if container {
			return ln, nil
		}
		multi := newMetricsMultiListener(port, ln)
		multi.addTailscale(portWait)
		go multi.watchTailscale(tailscaleRescanInterval)
		return multi, nil
	}
	return nil, fmt.Errorf("no free port in %s:%d-%d: %w", host, metricsAutoPorts[0], metricsAutoPorts[len(metricsAutoPorts)-1], lastErr)
}

// metricsServedAddrs lists the addresses /metrics is listening on, or nil
// when it is not running.
func metricsServedAddrs() []string {
	ln := metricsListener
	if ln == nil {
		return nil
	}
	if multi, ok := ln.(*metricsMultiListener); ok {
		return multi.Addrs()
	}
	return []string{ln.Addr().String()}
}

// applyMetricsListenLive rebinds a running /metrics listener after
// metrics_listen changed. Nothing to do when metrics is off: the next start
// reads the new setting.
func applyMetricsListenLive() error {
	if metricsServer == nil {
		return nil
	}
	if err := stopMetrics(); err != nil {
		return fmt.Errorf("metrics_listen: stopping the old listener: %w", err)
	}
	ln, err := listenMetrics(2 * time.Second)
	if err != nil {
		return fmt.Errorf("metrics_listen: %w", err)
	}
	if ln == nil {
		return nil
	}
	serveMetrics(ln)
	return nil
}

// metricsMultiListener serves one port on several addresses (loopback plus
// Tailscale) as a single net.Listener, so the HTTP server, stopMetrics and
// the HotSwap closer handle it like any other listener.
type metricsMultiListener struct {
	port  int
	conns chan net.Conn
	done  chan struct{}

	mu        sync.Mutex
	listeners []net.Listener
	failed    map[string]bool
	closed    bool
}

func newMetricsMultiListener(port int, first net.Listener) *metricsMultiListener {
	m := &metricsMultiListener{
		port:   port,
		conns:  make(chan net.Conn),
		done:   make(chan struct{}),
		failed: map[string]bool{},
	}
	m.add(first)
	return m
}

func (m *metricsMultiListener) add(ln net.Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		ln.Close()
		return
	}
	m.listeners = append(m.listeners, ln)
	go m.acceptLoop(ln)
}

func (m *metricsMultiListener) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient (EMFILE and the like): back off instead of spinning.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case m.conns <- conn:
		case <-m.done:
			conn.Close()
			return
		}
	}
}

func (m *metricsMultiListener) has(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ln := range m.listeners {
		if ln.Addr().String() == addr {
			return true
		}
	}
	return false
}

// addTailscale binds every Tailscale address not yet served. A failure is
// logged once per address, not on every rescan.
func (m *metricsMultiListener) addTailscale(wait time.Duration) {
	for _, ip := range tailscaleAddrsFunc() {
		addr := netip.AddrPortFrom(ip, uint16(m.port)).String()
		if m.has(addr) {
			continue
		}
		ln, err := listenOrWait(addr, wait)
		if err != nil {
			m.mu.Lock()
			first := !m.failed[addr]
			m.failed[addr] = true
			m.mu.Unlock()
			if first {
				tlog("[metrics] could not serve /metrics on Tailscale address %s: %v\n", addr, err)
			}
			continue
		}
		m.add(ln)
		tlog("[metrics] serving /metrics on Tailscale address %s\n", addr)
	}
}

func (m *metricsMultiListener) watchTailscale(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.addTailscale(0)
		}
	}
}

func (m *metricsMultiListener) Accept() (net.Conn, error) {
	select {
	case conn := <-m.conns:
		return conn, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *metricsMultiListener) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return net.ErrClosed
	}
	m.closed = true
	close(m.done)
	for _, ln := range m.listeners {
		ln.Close()
	}
	return nil
}

// Addr is the first (loopback) address.
func (m *metricsMultiListener) Addr() net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listeners[0].Addr()
}

func (m *metricsMultiListener) Addrs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.listeners))
	for _, ln := range m.listeners {
		out = append(out, ln.Addr().String())
	}
	return out
}
