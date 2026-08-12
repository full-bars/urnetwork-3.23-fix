package connect

import (
	"context"
	"encoding/json"
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
