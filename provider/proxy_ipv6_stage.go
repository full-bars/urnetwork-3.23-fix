package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/urnetwork/connect"
)

// proxyIPv4ControlTargets are the IPv4 endpoints for the fresh control dial
// that decides whether an IPv6 timeout counts as no.
var proxyIPv4ControlTargets = []string{"1.1.1.1", "8.8.8.8"}

// proxyIPv6StunServers are the STUN services for stage 3, resolved as IPv6.
var proxyIPv6StunServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}

// proxyIPv6Dials groups the functions of the three stages so a test can
// replace them and keep the probe deterministic.
type proxyIPv6Dials struct {
	connect func(context.Context, *connect.ProxySettings, string) (byte, error)
	control func(context.Context, *connect.ProxySettings, string) (byte, error)
	tcpExit func(context.Context, *connect.ProxySettings) (string, error)
	udpExit func(context.Context, *connect.ProxySettings) (string, error)
}

func defaultProxyIPv6Dials() proxyIPv6Dials {
	return proxyIPv6Dials{
		connect: probeProxyIPv6Connect,
		control: probeProxyIPv4Connect,
		tcpExit: proxyIPv6TCPExit,
		udpExit: proxyIPv6STUNExit,
	}
}

// probeProxyIPv4Connect opens a standalone SOCKS connection through the proxy
// to an IPv4 target. It is the same shape as the stage 1 probe, so a success
// proves the proxy forwards traffic in this run.
func probeProxyIPv4Connect(ctx context.Context, settings *connect.ProxySettings, target string) (byte, error) {
	ip := net.ParseIP(target)
	if ip == nil || ip.To4() == nil {
		return 0, errors.New("invalid IPv4 target")
	}
	network := settings.Network
	if network == "" {
		network = "tcp"
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, settings.Address)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	deadline := time.Now().Add(udpCheckReplyTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	user, password := "", ""
	if settings.Auth != nil {
		user, password = settings.Auth.User, settings.Auth.Password
	}
	if err := proxyIPv6Greet(conn, user, password); err != nil {
		return 0, err
	}
	request := []byte{5, 1, 0, 1}
	request = append(request, ip.To4()...)
	request = binary.BigEndian.AppendUint16(request, 443)
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	return readSocks5ConnectCode(conn)
}

// readSocks5ConnectCode reads a SOCKS5 CONNECT reply and returns its reply
// code. A malformed reply is an error.
func readSocks5ConnectCode(conn net.Conn) (byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return 0, err
	}
	if head[0] != 5 {
		return 0, errors.New("invalid SOCKS reply version")
	}
	if head[2] != 0 {
		return 0, errors.New("invalid SOCKS reply reserved byte")
	}
	var addressBytes int
	switch head[3] {
	case 1:
		addressBytes = 4
	case 4:
		addressBytes = 16
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return 0, err
		}
		addressBytes = int(length[0])
	default:
		return 0, errors.New("invalid SOCKS address type")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressBytes+2)); err != nil {
		return 0, err
	}
	return head[1], nil
}

// proxyIPv4ControlAlive runs the fresh control of the same run: one SOCKS
// connection through the proxy to an IPv4 target. A reply that proves the proxy
// forwarded to the target means it forwards traffic, so a timeout on IPv6 is a
// real IPv6 result.
func proxyIPv4ControlAlive(ctx context.Context, settings *connect.ProxySettings, targets []string, dial func(context.Context, *connect.ProxySettings, string) (byte, error)) bool {
	for _, target := range targets {
		if ctx.Err() != nil {
			return false
		}
		// only a reply that proves the proxy forwarded to the target counts: a
		// success, or a connection refused by the target itself. A ruleset
		// refusal or a network failure says the proxy did not forward
		if code, err := dial(ctx, settings, target); err == nil && (code == 0 || code == 5) {
			return true
		}
	}
	return false
}

// proxyIPv6TCPExit learns the IPv6 TCP exit address of the proxy (stage 2) by
// asking the IPv6 address service through the proxy.
func proxyIPv6TCPExit(ctx context.Context, settings *connect.ProxySettings) (string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, proxyIPv6ExitHost)
	if err != nil {
		return "", err
	}
	network := settings.Network
	if network == "" {
		network = "tcp"
	}
	var lastErr error
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			continue
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, settings.Address)
		if err != nil {
			return "", err
		}
		exit, err := proxyIPv6HTTPExitOnConn(ctx, conn, settings, addr.IP)
		conn.Close()
		if err == nil {
			return exit, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("the exit service has no IPv6 address")
	}
	return "", lastErr
}

// proxyIPv6STUNExit learns the IPv6 UDP exit address of the proxy (stage 3) by
// asking an IPv6 STUN service through the relay.
func proxyIPv6STUNExit(ctx context.Context, settings *connect.ProxySettings) (string, error) {
	pc, err := probeProxyUDPRelay(ctx, settings)
	if err != nil {
		return "", err
	}
	defer pc.Close()
	var lastErr error
	for _, server := range proxyIPv6StunServers {
		destination, err := resolveProxyIPv6UDPAddr(ctx, server)
		if err != nil {
			lastErr = err
			continue
		}
		exit, err := proxyIPv6UDPExit(ctx, pc, destination)
		if err == nil {
			return exit, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("the relay did not return an IPv6 STUN address")
	}
	return "", lastErr
}

// resolveProxyIPv6UDPAddr resolves a host:port to an IPv6 UDP address with the
// probe's context, so a slow lookup ends with the probe.
func resolveProxyIPv6UDPAddr(ctx context.Context, server string) (*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("invalid STUN port")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if addr.IP.To4() == nil {
			return &net.UDPAddr{IP: addr.IP, Port: port}, nil
		}
	}
	return nil, errors.New("the STUN host has no IPv6 address")
}

// runProxyIPv6Stage measures the IPv6 capability of one identity and returns
// the new result. The second return value is false when the run stopped before
// it could measure anything, so the caller keeps the previous result.
func runProxyIPv6Stage(ctx context.Context, settings *connect.ProxySettings, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (proxyIPv6Result, bool) {
	if ctx.Err() != nil {
		return previous, false
	}
	// Stage 1: connect-only probe without IPv4 control.
	status := probeProxyIPv6With(ctx, settings, false, proxyIPv6Targets, dials.connect)
	// If unknown, run a fresh IPv4 control. Only if the control succeeds does
	// a timeout in stage 1 count as "no" (ipv4Control=true).
	if status == proxyIPv6Unknown && ctx.Err() == nil {
		if proxyIPv4ControlAlive(ctx, settings, proxyIPv4ControlTargets, dials.control) {
			status = probeProxyIPv6With(ctx, settings, true, proxyIPv6Targets, dials.connect)
		}
	}
	// Stages 2 and 3 only run after a stage 1 yes.
	udp := "unknown"
	var tcpExit, udpExit string
	if status == proxyIPv6Yes {
		if exit, err := dials.tcpExit(ctx, settings); err == nil && exit != "" {
			tcpExit = exit
			if exit6, err := dials.udpExit(ctx, settings); err == nil && exit6 != "" {
				udpExit = exit6
				udp = proxyIPv6UDPStatus(tcpExit, udpExit)
			}
		}
	}
	// a run the context cut short observed nothing reliable: keep the previous
	// result instead of recording the interruption as an answer
	if ctx.Err() != nil {
		return previous, false
	}
	return previous.measured(now, status, udp, tcpExit, udpExit), true
}

// ipv6StageKey is the context key for carrying the stage state through the
// probe call chain.
type ipv6StageKey struct{}

// ipv6StageOnlyKey marks a check that should only run the IPv6 stage and skip
// the UDP relay check.
type ipv6StageOnlyKey struct{}

// ipv6Stage holds the parameters for a deferred IPv6 stage run.
type ipv6Stage struct {
	previous proxyIPv6Result
	now      time.Time
	dials    proxyIPv6Dials
	result   *proxyIPv6Result
	measured *bool
}

// ipv6StageFromContext retrieves the stage sink from context.
func ipv6StageFromContext(ctx context.Context) (ipv6Stage, bool) {
	stage, ok := ctx.Value(ipv6StageKey{}).(ipv6Stage)
	return stage, ok && stage.result != nil && stage.measured != nil
}

// withIPv6Stage marks a check run that must also verify IPv6, and returns the
// sink the probe writes its measurement into.
func withIPv6Stage(ctx context.Context, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (context.Context, *proxyIPv6Result, *bool) {
	result := new(proxyIPv6Result)
	measured := new(bool)
	stage := ipv6Stage{previous: previous, now: now, dials: dials, result: result, measured: measured}
	return context.WithValue(ctx, ipv6StageKey{}, stage), result, measured
}

// ipv6SweepKey marks a context that belongs to a check the background UDP sweep
// started. Only such checks run the IPv6 stage.
type ipv6SweepKey struct{}

func withIPv6Sweep(ctx context.Context) context.Context {
	return context.WithValue(ctx, ipv6SweepKey{}, true)
}

func ipv6SweepFromContext(ctx context.Context) bool {
	sweep, _ := ctx.Value(ipv6SweepKey{}).(bool)
	return sweep
}
