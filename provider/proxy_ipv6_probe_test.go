package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyIPv6HTTPExit(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{"2001:db8::42\n", "2001:db8::42"},
		{"192.0.2.1\n", ""},
		{"not an address", ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				var greeting [3]byte
				if _, err := io.ReadFull(server, greeting[:]); err != nil {
					done <- err
					return
				}
				if _, err := server.Write([]byte{5, 0}); err != nil {
					done <- err
					return
				}
				var request [22]byte
				if _, err := io.ReadFull(server, request[:]); err != nil {
					done <- err
					return
				}
				if request[3] != 4 || request[20] != 0 || request[21] != 80 {
					done <- errors.New("invalid target")
					return
				}
				if _, err := server.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					done <- err
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					done <- err
					return
				}
				if req.Host != proxyIPv6ExitHost {
					done <- errors.New("invalid host")
					return
				}
				_, err = fmt.Fprintf(server, "HTTP/1.0 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(tc.body), tc.body)
				done <- err
			}()
			got, err := proxyIPv6HTTPExitOnConn(context.Background(), client, &connect.ProxySettings{}, net.ParseIP("2001:db8::1"))
			client.Close()
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProxyIPv6Reply(t *testing.T) {
	for _, tc := range []struct {
		code byte
		want proxyIPv6Status
	}{
		{0, proxyIPv6Yes}, {1, proxyIPv6Unknown}, {2, proxyIPv6Unknown},
		{3, proxyIPv6No}, {4, proxyIPv6No}, {5, proxyIPv6Yes},
		{6, proxyIPv6Unknown}, {7, proxyIPv6Unknown}, {8, proxyIPv6No}, {255, proxyIPv6Unknown},
	} {
		if got := proxyIPv6Reply(tc.code); got != tc.want {
			t.Errorf("code %d: got %s, want %s", tc.code, got, tc.want)
		}
	}
}

func TestProxyIPv6ConnectHandshake(t *testing.T) {
	for _, code := range []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 255} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				var greeting [3]byte
				if _, err := io.ReadFull(server, greeting[:]); err != nil {
					done <- err
					return
				}
				if greeting != [3]byte{5, 1, 0} {
					done <- errors.New("invalid greeting")
					return
				}
				if _, err := server.Write([]byte{5, 0}); err != nil {
					done <- err
					return
				}
				var request [22]byte
				if _, err := io.ReadFull(server, request[:]); err != nil {
					done <- err
					return
				}
				if request[0] != 5 || request[1] != 1 || request[2] != 0 || request[3] != 4 || !net.IP(request[4:20]).Equal(net.ParseIP("2001:db8::1")) || request[20] != 1 || request[21] != 187 {
					done <- errors.New("invalid CONNECT request")
					return
				}
				if _, err := server.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					done <- err
					return
				}
				var payload [1]byte
				if n, err := server.Read(payload[:]); n != 0 || err != io.EOF {
					done <- errors.New("unexpected probe payload")
					return
				}
				done <- nil
			}()
			got, err := proxyIPv6ConnectOnConn(context.Background(), client, &connect.ProxySettings{}, net.ParseIP("2001:db8::1"))
			client.Close()
			if err != nil || got != code {
				t.Fatalf("reply = %d, %v; want %d", got, err, code)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProxyIPv6Interval(t *testing.T) {
	if got := proxyIPv6Interval(proxyIPv6Yes, proxyIPv6No, 48*time.Hour); got != 12*time.Hour {
		t.Fatal(got)
	}
	if got := proxyIPv6Interval(proxyIPv6Yes, proxyIPv6Yes, 12*time.Hour); got != 24*time.Hour {
		t.Fatal(got)
	}
	if got := proxyIPv6Interval(proxyIPv6Unknown, proxyIPv6Unknown, 8*time.Hour); got != 12*time.Hour {
		t.Fatal(got)
	}
}

func TestProxyIPv6MeasurementDueAndBackoff(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var result proxyIPv6Result
	if !result.due(now) {
		t.Fatal("unmeasured proxy must be due")
	}
	result = result.measured(now, proxyIPv6Yes, "unknown", "", "")
	if result.Interval != 12*time.Hour || result.Source != "measured" || result.due(now.Add(12*time.Hour-time.Nanosecond)) || !result.due(now.Add(12*time.Hour)) {
		t.Fatalf("first measurement: %+v", result)
	}
	result = result.measured(now.Add(12*time.Hour), proxyIPv6Yes, "yes", "2001:db8::1", "2001:db8::1")
	if result.Interval != 24*time.Hour {
		t.Fatalf("unchanged measurement interval: %s", result.Interval)
	}
	result = result.measured(now.Add(36*time.Hour), proxyIPv6No, "unknown", "", "")
	if result.Interval != 24*time.Hour || result.TCPExit != "" || result.UDPExit != "" {
		t.Fatalf("changed measurement: %+v", result)
	}
	result = result.measured(now.Add(60*time.Hour), proxyIPv6Unknown, "unknown", "", "")
	if result.Interval != time.Hour {
		t.Fatalf("unknown interval: %s", result.Interval)
	}
}

func TestProxyIPv6CancelledBeforeFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	dial := func(context.Context, *connect.ProxySettings, string) (byte, error) {
		calls++
		cancel()
		return 4, nil
	}
	got := probeProxyIPv6With(ctx, nil, true, []string{"first", "second"}, dial)
	if got != proxyIPv6Unknown || calls != 1 {
		t.Fatalf("got %s after %d attempts, want unknown after one", got, calls)
	}
}

type ipv6TestTimeout struct{}

func (ipv6TestTimeout) Error() string   { return "timeout" }
func (ipv6TestTimeout) Timeout() bool   { return true }
func (ipv6TestTimeout) Temporary() bool { return true }

func TestProxyIPv6FallbackAndTimeout(t *testing.T) {
	targets := []string{"first", "second"}
	for _, tc := range []struct {
		name    string
		control bool
		codes   []byte
		errs    []error
		want    proxyIPv6Status
		calls   int
	}{
		{"first succeeds", false, []byte{0}, []error{nil}, proxyIPv6Yes, 1},
		{"second succeeds", false, []byte{4, 5}, []error{nil, nil}, proxyIPv6Yes, 2},
		{"timeout with control", true, []byte{0, 0}, []error{proxyIPv6ConnectTimeout{ipv6TestTimeout{}}, proxyIPv6ConnectTimeout{ipv6TestTimeout{}}}, proxyIPv6No, 2},
		{"timeout without control", false, []byte{0, 0}, []error{proxyIPv6ConnectTimeout{ipv6TestTimeout{}}, proxyIPv6ConnectTimeout{ipv6TestTimeout{}}}, proxyIPv6Unknown, 2},
		{"proxy timeout with control", true, []byte{0, 0}, []error{ipv6TestTimeout{}, ipv6TestTimeout{}}, proxyIPv6Unknown, 2},
		{"inconclusive fallback", true, []byte{4, 1}, []error{nil, nil}, proxyIPv6Unknown, 2},
		{"other failure", true, []byte{0, 0}, []error{errors.New("failure"), errors.New("failure")}, proxyIPv6Unknown, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			dial := func(_ context.Context, _ *connect.ProxySettings, target string) (byte, error) {
				if target != targets[calls] {
					t.Fatalf("unexpected target %s", target)
				}
				i := calls
				calls++
				return tc.codes[i], tc.errs[i]
			}
			if got := probeProxyIPv6With(context.Background(), nil, tc.control, targets, dial); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if calls != tc.calls {
				t.Fatalf("got %d calls, want %d", calls, tc.calls)
			}
		})
	}
}
