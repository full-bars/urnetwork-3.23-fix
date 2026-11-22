package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

func TestPlatformRetryAfterReadsBothFormsWithinTheCap(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		value string
		want  time.Duration
	}{
		{value: "", want: 0},
		{value: "7", want: 7 * time.Second},
		{value: " 7 ", want: 7 * time.Second},
		{value: "60", want: 60 * time.Second},
		{value: "61", want: PlatformRetryAfterMaxTimeout},
		{value: "999999999999", want: PlatformRetryAfterMaxTimeout},
		{value: "0", want: 0},
		{value: "-5", want: 0},
		{value: "soon", want: 0},
		{value: now.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second},
		{value: now.Add(time.Hour).Format(http.TimeFormat), want: PlatformRetryAfterMaxTimeout},
		{value: now.Add(-time.Hour).Format(http.TimeFormat), want: 0},
	} {
		header := http.Header{}
		if c.value != "" {
			header.Set("Retry-After", c.value)
		}
		if got := platformRetryAfter(header, now); got != c.want {
			t.Errorf("Retry-After %q = %s, want %s", c.value, got, c.want)
		}
	}
}

func TestPlatformDialRefusalWait(t *testing.T) {
	rejection := func(statusCode int, retryAfter time.Duration) *HTTPUpgradeError {
		return &HTTPUpgradeError{StatusCode: statusCode, Reason: "rejected", RetryAfter: retryAfter}
	}
	capClose := &quic.ApplicationError{Remote: true, ErrorCode: quicConnectionCapCloseCode, ErrorMessage: "too many connections"}
	noJitter := func(time.Duration) time.Duration { return 0 }
	allJitter := func(maxJitter time.Duration) time.Duration { return maxJitter }

	for _, c := range []struct {
		name string
		err  error
		draw func(time.Duration) time.Duration
		want time.Duration
	}{
		{name: "no error", err: nil, want: 0},
		{name: "a deadline", err: context.DeadlineExceeded, want: 0},
		{name: "a 429", err: rejection(429, 7*time.Second), want: 7 * time.Second},
		{name: "a 429 without Retry-After", err: rejection(429, 0), want: 0},
		{name: "a 503 with a wait", err: rejection(503, 7*time.Second), want: 0},
		{name: "a 401", err: &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true}, want: 0},
		{name: "a 429 asking for too long", err: rejection(429, time.Hour), want: PlatformRetryAfterMaxTimeout},
		{
			name: "the longest of an exhausted strategy's refusals",
			err:  &HttpRequestExhaustedError{causes: []error{io.EOF, rejection(429, 3*time.Second), errors.Join(errors.New("websocket: bad handshake"), rejection(429, 8*time.Second)), context.DeadlineExceeded}},
			want: 8 * time.Second,
		},
		{name: "a wrapped 429", err: fmt.Errorf("dial: %w", rejection(429, 4*time.Second)), want: 4 * time.Second},
		{name: "the cap close, least jitter", err: capClose, draw: noJitter, want: 5 * time.Second},
		{name: "the cap close, most jitter", err: capClose, draw: allJitter, want: 30*time.Second - 1},
		{name: "the cap close, wrapped", err: fmt.Errorf("h3 open stream: %w", capClose), draw: noJitter, want: 5 * time.Second},
		{name: "the cap close beside a longer 429", err: errors.Join(capClose, rejection(429, 40*time.Second)), draw: noJitter, want: 40 * time.Second},
		{name: "the cap code closed locally", err: &quic.ApplicationError{Remote: false, ErrorCode: quicConnectionCapCloseCode}, draw: noJitter, want: 0},
		{name: "the unknown name close", err: &quic.ApplicationError{Remote: true, ErrorCode: 0x1000}, draw: noJitter, want: 0},
		{name: "the client limit close", err: &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode}, draw: noJitter, want: 0},
	} {
		if got := platformDialRefusalWait(c.err, c.draw); got != c.want {
			t.Errorf("%s: wait %s, want %s", c.name, got, c.want)
		}
	}
	// the real draw stays in [5 s, 30 s)
	for range 64 {
		if wait := platformDialRefusalWait(capClose, nil); wait < 5*time.Second || 30*time.Second <= wait {
			t.Fatalf("the cap close's wait %s, want in [5s, 30s)", wait)
		}
	}
}

func TestDialRetryAfterPreservesPinnedBackoff(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	tr := newPinnedRouteTestTransport()
	tr.noteDialFailure(context.DeadlineExceeded)
	if tr.pinnedBackoff.delay() <= 0 {
		t.Fatal("pinned failure did not retain its own backoff")
	}
	refusal := newHTTPUpgradeRejection(429, http.Header{"Retry-After": {"120"}}, false)
	if got := tr.dialRetryDelay(NewReconnect(0), refusal); got != time.Minute {
		t.Fatalf("pinned delay = %s", got)
	}
	if got := tr.dialRetryDelay(NewReconnect(0), nil); got != 0 {
		t.Fatalf("pinned reconnect changed: %s", got)
	}
}

func TestHttpUpgradeRetryAfter(t *testing.T) {
	for _, value := range []string{"2", ""} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if value != "" {
					w.Header().Set("Retry-After", value)
				}
				http.Error(w, "limited", 429)
			}))
			defer server.Close()
			for _, framed := range []bool{false, true} {
				_, err := DialH1Messages(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), http.Header{}, &websocket.Dialer{HandshakeTimeout: time.Second}, H1FramerProtocol, 65535, framed, nil)
				var rejection *HTTPUpgradeError
				if !errors.As(err, &rejection) {
					t.Fatalf("framed %t: %v", framed, err)
				}
				want := time.Duration(0)
				if value != "" {
					want = 2 * time.Second
				}
				if rejection.RetryAfter != want {
					t.Fatalf("framed %t: got %s want %s", framed, rejection.RetryAfter, want)
				}
				if rejection.Terminal != (want > 0) {
					t.Fatalf("terminal = %t", rejection.Terminal)
				}
			}
		})
	}
}

func TestPlatformTransportRetryAfterDelaysNextDial(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	requests := make(chan time.Time, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- time.Now():
		default:
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	tr := newFlapTestTransport(t, ctx, server.URL, settings)
	defer tr.Close()
	var first time.Time
	select {
	case first = <-requests:
	case <-time.After(10 * time.Second):
		t.Fatal("first dial missing")
	}
	TriggerPulse()
	select {
	case second := <-requests:
		if second.Sub(first) < 2*time.Second {
			t.Fatalf("redial after %s", second.Sub(first))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second dial missing")
	}
}

func TestDialRetryAfterConnectionCapAndOrdinaryBackoff(t *testing.T) {
	tr := &PlatformTransport{failBackoff: newDialFailureBackoff(time.Hour, 4*time.Hour)}
	capClose := &quic.ApplicationError{Remote: true, ErrorCode: quicConnectionCapCloseCode}
	if d := tr.dialRetryDelay(NewReconnect(0), capClose); d < 5*time.Second || d >= 30*time.Second {
		t.Fatalf("QUIC retry delay = %s", d)
	}
	if d := tr.dialRetryDelay(NewReconnect(0), errors.New("ordinary failure")); d != 0 {
		t.Fatalf("ordinary retry delay = %s", d)
	}
	tr.failBackoff.fail()
	tr.failBackoff.fail()
	if d := tr.dialRetryDelay(NewReconnect(0), errors.New("ordinary failure")); d < 30*time.Minute {
		t.Fatalf("failure backoff = %s", d)
	}
}

func TestPlatformTransportRetryAfterQUICConnectionCap(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	udp := listenLoopbackUDP(t)
	port := udp.LocalAddr().(*net.UDPAddr).Port
	listener, err := quic.Listen(udp, selfSignedTlsConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connections := make(chan time.Time, 16)
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func(c *quic.Conn) {
				if _, err := c.AcceptStream(ctx); err != nil {
					return
				}
				select {
				case connections <- time.Now():
				default:
				}
				c.CloseWithError(quicConnectionCapCloseCode, "too many connections")
			}(conn)
		}
	}()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 5 * time.Millisecond
	settings.H3Port = port
	settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"h3count"}}
	ss := DefaultClientStrategySettings()
	ss.MinNextConnectDelay = 0
	ss.MaxNextConnectDelay = 0
	ss.RequestTimeout = 500 * time.Millisecond
	strategy := NewClientStrategy(ctx, ss)
	auth := &ClientAuth{ByJwt: "jwt", InstanceId: NewId(), AppVersion: "1.0.0", ProvideIntent: true}
	tr := NewPlatformTransportWithTargetMode(ctx, strategy, NewRouteManager(ctx, "test-cap"), "https://localhost", auth, TransportModeH3, settings)
	defer tr.Close()
	select {
	case <-connections:
	case <-time.After(15 * time.Second):
		t.Fatal("first QUIC connection missing")
	}
	select {
	case <-connections:
		t.Fatal("QUIC redial before the 5s minimum")
	case <-time.After(3 * time.Second):
	}
}
