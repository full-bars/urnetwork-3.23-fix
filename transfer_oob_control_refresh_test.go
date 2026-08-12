package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestApiOutOfBandControlUsesRefreshedJwt(t *testing.T) {
	authorizations := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		var args ConnectControlArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "old-jwt", server.URL)

	send := func() {
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
			done <- err
		})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("out-of-band control request timed out")
		}
	}

	send()
	control.SetByJwt("new-jwt")
	send()

	for i, want := range []string{"Bearer old-jwt", "Bearer new-jwt"} {
		select {
		case got := <-authorizations:
			if got != want {
				t.Fatalf("request %d authorization = %q, want %q", i+1, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing authorization for request %d", i+1)
		}
	}
}

func TestApiOutOfBandControlCounts401s(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "jwt", server.URL)

	for i := 0; i < 3; i++ {
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) { done <- err })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("send timed out")
		}
	}

	if got := control.Audit401Count(); got != 3 {
		t.Fatalf("Audit401Count = %d, want 3", got)
	}
	control.ResetAudit401Count()
	if got := control.Audit401Count(); got != 0 {
		t.Fatalf("Audit401Count after reset = %d, want 0", got)
	}
}

func TestApiOutOfBandControlOn401Callback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "jwt", server.URL)

	// The renewal watcher registers a callback that pings its renew-now
	// channel; assert it fires exactly once per 401.
	notify := make(chan struct{}, 4)
	control.SetOn401(func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	})

	for i := 0; i < 3; i++ {
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) { done <- err })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("send timed out")
		}
	}

	for i := 0; i < 3; i++ {
		select {
		case <-notify:
		case <-time.After(5 * time.Second):
			t.Fatalf("on401 callback did not fire for 401 #%d", i+1)
		}
	}

	// A successful (non-401) send must NOT fire the callback.
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args ConnectControlArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack})
	}))
	defer okServer.Close()
	okControl := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "jwt", okServer.URL)
	okNotify := make(chan struct{}, 1)
	okControl.SetOn401(func() {
		select {
		case okNotify <- struct{}{}:
		default:
		}
	})
	done := make(chan error, 1)
	okControl.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) { done <- err })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ok send timed out")
	}
	select {
	case <-okNotify:
		t.Fatal("on401 callback fired for a successful send")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestApiOutOfBandControlDoesNotCountNon401ErrorsAsUnauthorized is an
// integration-level companion to TestIsUnauthorizedError: a real 502 response
// whose body happens to mention "401" must not increment audit401Count or
// fire the on401 callback, since SendControl feeds errors through
// isUnauthorizedError before counting them.
func TestApiOutOfBandControlDoesNotCountNon401ErrorsAsUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream returned 401 for port 401"))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "jwt", server.URL)

	notify := make(chan struct{}, 1)
	control.SetOn401(func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	})

	done := make(chan error, 1)
	control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) { done <- err })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error for a 502 response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send timed out")
	}

	if got := control.Audit401Count(); got != 0 {
		t.Fatalf("Audit401Count = %d after a non-401 error, want 0", got)
	}
	select {
	case <-notify:
		t.Fatal("on401 callback fired for a non-401 (502) error")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestApiOutOfBandControlSetOn401Nil pins that SetOn401(nil) clears any
// previously registered callback rather than panicking on the next 401.
func TestApiOutOfBandControlSetOn401Nil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "jwt", server.URL)

	notify := make(chan struct{}, 1)
	control.SetOn401(func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	})
	// Clear it before any 401 arrives.
	control.SetOn401(nil)

	done := make(chan error, 1)
	control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) { done <- err })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send timed out")
	}

	// The counter must still increment even with no callback registered.
	if got := control.Audit401Count(); got != 1 {
		t.Fatalf("Audit401Count = %d, want 1", got)
	}
	select {
	case <-notify:
		t.Fatal("on401 callback fired after being cleared with SetOn401(nil)")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestIsUnauthorizedError pins the false-positive fix: only the canonical
// "401 Unauthorized" status line matches — bare "401" or "Unauthorized" in
// a body must NOT (a "502 Bad Gateway: upstream returned 401 for port 401"
// error would otherwise trigger a spurious renewal storm).
func TestIsUnauthorizedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canonical 401", errors.New("401 Unauthorized: bad token"), true},
		{"wrapped canonical 401", fmt.Errorf("renewal: %w", errors.New("401 Unauthorized: bad token")), true},
		{"bare 401 in body", errors.New("502 Bad Gateway: upstream returned 401 for port 401"), false},
		{"bare Unauthorized in body", errors.New("400 Bad Request: unauthorized access attempt"), false},
		{"200 ok", errors.New("200 OK"), false},
		{"empty", errors.New(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthorizedError(tc.err); got != tc.want {
				t.Fatalf("isUnauthorizedError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
