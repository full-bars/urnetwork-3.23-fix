package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

type proxyIPv6Status string

// proxyIPv6Result is kept per proxy identity. A zero value represents an
// identity that has not yet been probed.
type proxyIPv6Result struct {
	Status   proxyIPv6Status `json:"status,omitempty"`
	Source   string          `json:"source,omitempty"`
	UDP      string          `json:"udp,omitempty"`
	TCPExit  string          `json:"tcp_exit,omitempty"`
	UDPExit  string          `json:"udp_exit,omitempty"`
	At       time.Time       `json:"at,omitempty"`
	Interval time.Duration   `json:"interval,omitempty"`
}

func (r proxyIPv6Result) due(now time.Time) bool {
	return r.At.IsZero() || r.Interval <= 0 || !now.Before(r.At.Add(r.Interval))
}

// retain keeps previous evidence long enough to calculate the next interval.
func (r proxyIPv6Result) retain(now time.Time) bool {
	return !r.At.IsZero() && !r.At.After(now) && r.Interval > 0 && now.Sub(r.At) < 2*r.Interval
}

// valid checks persisted data before exposing it to callers.
func (r proxyIPv6Result) valid(now time.Time) bool {
	if r.At.IsZero() {
		return r == (proxyIPv6Result{})
	}
	if r.At.After(now) || r.Interval <= 0 || r.Interval > 7*24*time.Hour || (r.Status != proxyIPv6Yes && r.Status != proxyIPv6No && r.Status != proxyIPv6Unknown) || (r.Source != "measured" && r.Source != "observed") {
		return false
	}
	if r.UDP != "" && r.UDP != "yes" && r.UDP != "no" && r.UDP != "unknown" && r.UDP != "mismatch" {
		return false
	}
	for _, raw := range []string{r.TCPExit, r.UDPExit} {
		if raw != "" {
			ip := net.ParseIP(raw)
			if ip == nil || ip.To4() != nil {
				return false
			}
		}
	}
	return true
}

// measured records a new observation and calculates its next check without
// deriving a result from any other proxy.
func (r proxyIPv6Result) measured(now time.Time, status proxyIPv6Status, udp, tcpExit, udpExit string) proxyIPv6Result {
	interval := proxyIPv6Interval(status, r.Status, r.Interval)
	if status == proxyIPv6Yes && udp == "unknown" {
		// the UDP exit is not established yet. Retry soon and do not back off,
		// because ipv6UDPAllowed needs a yes and a long interval would keep the
		// IPv6 UDP relay refused after one failed lookup.
		interval = time.Hour
	}
	return proxyIPv6Result{
		Status: status, Source: "measured", UDP: udp, TCPExit: tcpExit, UDPExit: udpExit,
		At: now, Interval: interval,
	}
}

const (
	proxyIPv6Unknown proxyIPv6Status = "unknown"
	proxyIPv6Yes     proxyIPv6Status = "yes"
	proxyIPv6No      proxyIPv6Status = "no"
)

var proxyIPv6Targets = []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}

const proxyIPv6ExitHost = "api6.ipify.org"

// proxyIPv6ConnectTimeout identifies a timeout after the CONNECT request was
// sent, rather than a timeout reaching or authenticating to the proxy.
type proxyIPv6ConnectTimeout struct{ error }

// proxyIPv6Reply classifies only replies that establish reachability or a
// specific IPv6 routing failure. Other errors do not establish capability.
func proxyIPv6Reply(code byte) proxyIPv6Status {
	switch code {
	case 0, 5:
		return proxyIPv6Yes
	case 3, 4, 8:
		return proxyIPv6No
	default:
		return proxyIPv6Unknown
	}
}

func proxyIPv6Interval(status, previous proxyIPv6Status, previousInterval time.Duration) time.Duration {
	base, cap := time.Hour, 12*time.Hour
	switch status {
	case proxyIPv6Yes:
		base, cap = 12*time.Hour, 7*24*time.Hour
	case proxyIPv6No:
		base, cap = 24*time.Hour, 7*24*time.Hour
	}
	if status != previous || previousInterval < base {
		return base
	}
	if previousInterval >= cap/2 {
		return cap
	}
	return previousInterval * 2
}

// probeProxyIPv6Connect opens a standalone SOCKS connection. It sends no
// application payload and does not enter the tracked data path.
func probeProxyIPv6Connect(ctx context.Context, settings *connect.ProxySettings, target string) (byte, error) {
	ip := net.ParseIP(target)
	if ip == nil || ip.To4() != nil {
		return 0, errors.New("invalid IPv6 target")
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
	return proxyIPv6ConnectOnConn(ctx, conn, settings, ip)
}

func proxyIPv6ConnectOnConn(ctx context.Context, conn net.Conn, settings *connect.ProxySettings, ip net.IP) (byte, error) {
	return proxyIPv6ConnectOnConnPort(ctx, conn, settings, ip, 443)
}

func proxyIPv6ConnectOnConnPort(ctx context.Context, conn net.Conn, settings *connect.ProxySettings, ip net.IP, port uint16) (byte, error) {
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
	request := []byte{5, 1, 0, 4}
	request = append(request, ip.To16()...)
	request = binary.BigEndian.AppendUint16(request, port)
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	// the reply gets a full window that starts now, so the time spent on the
	// greeting does not shorten it. The caller deadline stays a hard limit, and
	// a reply cut short by it proves nothing about the proxy
	replyDeadline := time.Now().Add(udpCheckReplyTimeout)
	cutByCaller := false
	if d, ok := ctx.Deadline(); ok && d.Before(replyDeadline) {
		replyDeadline = d
		cutByCaller = true
	}
	if err := conn.SetReadDeadline(replyDeadline); err != nil {
		return 0, err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() && !cutByCaller {
			return 0, proxyIPv6ConnectTimeout{err}
		}
		return 0, err
	}
	if header[0] != 5 || header[2] != 0 {
		return 0, errors.New("invalid SOCKS reply")
	}
	var addressBytes int
	switch header[3] {
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
	return header[1], nil
}

// proxyIPv6HTTPExitOnConn uses a previously resolved IPv6 address so each
// proxy receives the same destination without doing its own DNS lookup.
func proxyIPv6HTTPExitOnConn(ctx context.Context, conn net.Conn, settings *connect.ProxySettings, ip net.IP) (string, error) {
	if ip == nil || ip.To4() != nil || ip.To16() == nil {
		return "", errors.New("invalid IPv6 target")
	}
	code, err := proxyIPv6ConnectOnConnPort(ctx, conn, settings, ip, 80)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", errors.New("SOCKS CONNECT refused")
	}
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n", proxyIPv6ExitHost); err != nil {
		return "", err
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, tcpExitReplyMaxBytes)), nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("address service rejected request")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	exit := net.ParseIP(strings.TrimSpace(string(body)))
	if exit == nil || exit.To4() != nil {
		return "", errors.New("address service returned no IPv6 address")
	}
	return exit.String(), nil
}

// proxyIPv6Greet retains transport errors for timeout classification.
func proxyIPv6Greet(conn net.Conn, user, password string) error {
	if len(user) > 255 || len(password) > 255 {
		return errors.New("invalid SOCKS credentials")
	}
	greeting := []byte{5, 1, 0}
	if user != "" && password != "" {
		greeting = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	var response [2]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return err
	}
	if response[0] != 5 {
		return errors.New("invalid SOCKS greeting")
	}
	switch response[1] {
	case 0:
		return nil
	case 2:
		if user == "" || password == "" {
			return errors.New("SOCKS authentication unavailable")
		}
		auth := append([]byte{1, byte(len(user))}, user...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			return err
		}
		if response != [2]byte{1, 0} {
			return errors.New("SOCKS authentication failed")
		}
		return nil
	default:
		return errors.New("SOCKS authentication unavailable")
	}
}

func probeProxyIPv6(ctx context.Context, settings *connect.ProxySettings, ipv4Control bool) proxyIPv6Status {
	return probeProxyIPv6With(ctx, settings, ipv4Control, proxyIPv6Targets, probeProxyIPv6Connect)
}

func probeProxyIPv6With(ctx context.Context, settings *connect.ProxySettings, ipv4Control bool, targets []string, dial func(context.Context, *connect.ProxySettings, string) (byte, error)) proxyIPv6Status {
	allNo := len(targets) > 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return proxyIPv6Unknown
		}
		code, err := dial(ctx, settings, target)
		if err == nil {
			status := proxyIPv6Reply(code)
			if status == proxyIPv6Yes {
				return status
			}
			if status != proxyIPv6No {
				allNo = false
			}
		} else {
			var connectTimeout proxyIPv6ConnectTimeout
			if !ipv4Control || !errors.As(err, &connectTimeout) {
				allNo = false
			}
		}
	}
	if allNo {
		return proxyIPv6No
	}
	return proxyIPv6Unknown
}
