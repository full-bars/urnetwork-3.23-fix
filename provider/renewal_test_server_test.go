package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// renewalTestServer fakes both endpoints the watcher talks to:
//   - POST /network/auth-client  -> returns a fresh client JWT (same client_id)
//   - POST /connect/control      -> returns 401 when force401 is set, else 200
type renewalTestServer struct {
	t        *testing.T
	srv      *httptest.Server
	force401 atomic.Bool
	// concurrent counts overlapping /network/auth-client requests
	concurrent   atomic.Int32
	maxConcurrent atomic.Int32
	// clientIdSeen is the last ClientId the auth-client request carried
	clientIdSeen atomic.Value // string
}

func newRenewalTestServer(t *testing.T) *renewalTestServer {
	ts := &renewalTestServer{t: t}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/network/auth-client":
			cur := ts.concurrent.Add(1)
			defer ts.concurrent.Add(-1)
			for {
				max := ts.maxConcurrent.Load()
				if cur <= max || ts.maxConcurrent.CompareAndSwap(max, cur) {
					break
				}
			}

			var args connect.AuthNetworkClientArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				t.Errorf("auth-client decode: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if args.ClientId == nil {
				t.Errorf("auth-client renewal must carry ClientId")
				http.Error(w, "missing client_id", http.StatusBadRequest)
				return
			}
			ts.clientIdSeen.Store(args.ClientId.String())
			claims := map[string]interface{}{
				"client_id": args.ClientId.String(),
				"exp":       float64(time.Now().Add(24 * time.Hour).Unix()),
			}
			_ = json.NewEncoder(w).Encode(&connect.AuthNetworkClientResult{
				ByClientJwt: createFakeJWTWithClaims(claims),
			})
		default:
			// OOB control endpoint
			if ts.force401.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(&connect.ConnectControlResult{})
				return
			}
			var args connect.ConnectControlArgs
			_ = json.NewDecoder(r.Body).Decode(&args)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&connect.ConnectControlResult{Pack: args.Pack})
		}
	}))
	return ts
}

// forceOob401 sends one OOB control request through oob and asserts the fake
// server answers 401 (which bumps oob's Audit401Count).
func (ts *renewalTestServer) forceOob401(oob *connect.ApiOutOfBandControl) error {
	ts.force401.Store(true)
	defer ts.force401.Store(false)
	done := make(chan error, 1)
	oob.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
		done <- err
	})
	select {
	case err := <-done:
		if err == nil {
			ts.t.Errorf("expected 401 error from OOB send")
			return nil
		}
		return nil
	case <-time.After(5 * time.Second):
		ts.t.Errorf("OOB send timed out")
		return nil
	}
}

// assertMaxConcurrent asserts that at most n auth-client requests overlapped.
func (ts *renewalTestServer) assertMaxConcurrent(t *testing.T, n int32) {
	t.Helper()
	if got := ts.maxConcurrent.Load(); got > n {
		t.Fatalf("max concurrent auth-client requests = %d, want <= %d", got, n)
	}
}
