package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The fake server below answers the two refresh flows the provider can use:
//
//   - POST /auth/network-refresh, the route under test (default: 404, i.e. a
//     server that predates it)
//   - POST /auth/code-create, POST /auth/code-login and GET /transfer/stats,
//     the legacy three-step flow
//
// Every request is recorded with the bearer token it carried, so a test can
// assert which token species was presented to which route.

type networkRefreshRequest struct {
	Method string
	Path   string
	Auth   string
	Body   string
}

type networkRefreshRequestLog struct {
	mu       sync.Mutex
	requests []networkRefreshRequest
}

func (self *networkRefreshRequestLog) record(r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	self.mu.Lock()
	defer self.mu.Unlock()
	self.requests = append(self.requests, networkRefreshRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Auth:   auth,
		Body:   string(body),
	})
}

func (self *networkRefreshRequestLog) snapshot() []networkRefreshRequest {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]networkRefreshRequest(nil), self.requests...)
}

func (self *networkRefreshRequestLog) count(path string) int {
	count := 0
	for _, request := range self.snapshot() {
		if request.Path == path {
			count += 1
		}
	}
	return count
}

func (self *networkRefreshRequestLog) authFor(path string) string {
	for _, request := range self.snapshot() {
		if request.Path == path {
			return request.Auth
		}
	}
	return ""
}

func (self *networkRefreshRequestLog) paths() []string {
	paths := []string{}
	for _, request := range self.snapshot() {
		paths = append(paths, request.Path)
	}
	return paths
}

type fakeNetworkAuthServer struct {
	t   *testing.T
	srv *httptest.Server
	log *networkRefreshRequestLog

	// renewedJWT is the token the code-login response carries. The
	// network-refresh handler defaults to 404, a server without the route.
	renewedJWT string

	networkRefresh func(w http.ResponseWriter, r *http.Request)
	codeCreate     func(w http.ResponseWriter, r *http.Request)
	codeLogin      func(w http.ResponseWriter, r *http.Request)
	transferStats  func(w http.ResponseWriter, r *http.Request)
}

func newFakeNetworkAuthServer(t *testing.T, renewedJWT string) *fakeNetworkAuthServer {
	t.Helper()
	fake := &fakeNetworkAuthServer{
		t:          t,
		log:        &networkRefreshRequestLog{},
		renewedJWT: renewedJWT,
	}
	fake.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.log.record(r)
		switch r.URL.Path {
		case "/auth/network-refresh":
			if fake.networkRefresh != nil {
				fake.networkRefresh(w, r)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case "/auth/code-create":
			if fake.codeCreate != nil {
				fake.codeCreate(w, r)
				return
			}
			writeNetworkRefreshJSON(t, w, map[string]any{"auth_code": "fake-auth-code"})
		case "/auth/code-login":
			if fake.codeLogin != nil {
				fake.codeLogin(w, r)
				return
			}
			writeNetworkRefreshJSON(t, w, map[string]any{"by_jwt": fake.renewedJWT})
		case "/transfer/stats":
			if fake.transferStats != nil {
				fake.transferStats(w, r)
				return
			}
			writeNetworkRefreshJSON(t, w, map[string]any{})
		case "/hello":
			writeNetworkRefreshJSON(t, w, map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.srv.Close)
	return fake
}

func writeNetworkRefreshJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

func networkRefreshTestNetworkJWT() string {
	return createFakeJWTWithClaims(map[string]interface{}{
		"network_id":   "net-1",
		"user_id":      "user-1",
		"network_name": "testnet",
		"exp":          float64(time.Now().Add(30 * 24 * time.Hour).Unix()),
	})
}

func networkRefreshTestClientJWT() string {
	return createFakeJWTWithClaims(map[string]interface{}{
		"network_id":   "net-1",
		"user_id":      "user-1",
		"network_name": "testnet",
		"client_id":    "client-1",
		"device_id":    "device-1",
		"exp":          float64(time.Now().Add(24 * time.Hour).Unix()),
	})
}

func TestNetworkRefreshFallbackStatus(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusConflict, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusNotImplemented, true},
		{http.StatusRequestTimeout, false},
		{http.StatusTooEarly, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, test := range tests {
		if got := networkRefreshFallbackStatus(test.statusCode); got != test.want {
			t.Errorf("networkRefreshFallbackStatus(%d) = %v, want %v", test.statusCode, got, test.want)
		}
	}
}

func TestNetworkRefreshJWTNeverPresentsAClientToken(t *testing.T) {
	fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
	strategy := connect.NewClientStrategyWithDefaults(context.Background())

	_, fallback, err := networkRefreshJWT(context.Background(), strategy, fake.srv.URL, networkRefreshTestClientJWT())
	if err == nil {
		t.Fatal("networkRefreshJWT accepted a client JWT")
	}
	if !fallback {
		t.Fatalf("fallback = false, want true (error %v)", err)
	}
	if !strings.Contains(err.Error(), "client JWT") {
		t.Errorf("error = %v, want a client JWT explanation", err)
	}
	if got := fake.log.count("/auth/network-refresh"); got != 0 {
		t.Fatalf("network-refresh requests = %d, want 0: a client token must never be presented", got)
	}
}

func TestNetworkRefreshJWTRequiresAToken(t *testing.T) {
	fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
	strategy := connect.NewClientStrategyWithDefaults(context.Background())

	_, fallback, err := networkRefreshJWT(context.Background(), strategy, fake.srv.URL, "")
	if err == nil || !fallback {
		t.Fatalf("networkRefreshJWT(\"\") = fallback %v, err %v; want a fallback error", fallback, err)
	}
	if got := fake.log.count("/auth/network-refresh"); got != 0 {
		t.Fatalf("network-refresh requests = %d, want 0", got)
	}
}

func TestRefreshJWTPrefersNetworkRefresh(t *testing.T) {
	original := networkRefreshTestNetworkJWT()
	renewed := networkRefreshTestNetworkJWT()
	fake := newFakeNetworkAuthServer(t, "legacy-token-should-not-be-used")
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		writeNetworkRefreshJSON(t, w, map[string]any{"by_jwt": renewed})
	}

	got, err := refreshJWT(context.Background(), fake.srv.URL, original)
	if err != nil {
		t.Fatalf("refreshJWT: %v", err)
	}
	if got != renewed {
		t.Fatalf("refreshed token = %q, want the network-refresh token", got)
	}

	requests := fake.log.snapshot()
	if len(requests) == 0 {
		t.Fatal("no requests reached the fake server")
	}
	if requests[0].Path != "/auth/network-refresh" {
		t.Fatalf("first request path = %q, want /auth/network-refresh", requests[0].Path)
	}
	if requests[0].Method != http.MethodPost {
		t.Errorf("network-refresh method = %q, want POST", requests[0].Method)
	}
	if requests[0].Auth != original {
		t.Errorf("network-refresh carried %q, want the network JWT", requests[0].Auth)
	}
	if jwtContainsClientId(requests[0].Auth) {
		t.Error("network-refresh was presented a client JWT")
	}
	if got := fake.log.count("/auth/code-create"); got != 0 {
		t.Errorf("code-create requests = %d, want 0 when network-refresh succeeds", got)
	}
	if got := fake.log.count("/auth/code-login"); got != 0 {
		t.Errorf("code-login requests = %d, want 0 when network-refresh succeeds", got)
	}
	if got := fake.log.count("/transfer/stats"); got != 1 {
		t.Errorf("transfer/stats requests = %d, want 1 (the token is verified before it is used)", got)
	}
	if got := fake.log.authFor("/transfer/stats"); got != renewed {
		t.Errorf("verification carried %q, want the renewed network JWT", got)
	}
}

func TestRefreshJWTFallsBackWhenNetworkRefreshIsMissing(t *testing.T) {
	original := networkRefreshTestNetworkJWT()
	renewed := networkRefreshTestNetworkJWT()
	fake := newFakeNetworkAuthServer(t, renewed) // /auth/network-refresh defaults to 404

	got, err := refreshJWT(context.Background(), fake.srv.URL, original)
	if err != nil {
		t.Fatalf("refreshJWT: %v", err)
	}
	if got != renewed {
		t.Fatalf("refreshed token = %q, want the legacy code-login token", got)
	}

	wantPaths := []string{"/auth/network-refresh", "/auth/code-create", "/auth/code-login", "/transfer/stats"}
	if gotPaths := fake.log.paths(); !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("request order = %v, want %v", gotPaths, wantPaths)
	}
	if got := fake.log.authFor("/auth/code-create"); got != original {
		t.Errorf("code-create carried %q, want the network JWT", got)
	}
	if authCode := fake.log.snapshot()[2].Body; !strings.Contains(authCode, "fake-auth-code") {
		t.Errorf("code-login body = %q, want the auth code from code-create", authCode)
	}
}

func TestRefreshJWTFallsBackWhenNetworkRefreshRefuses(t *testing.T) {
	for _, statusCode := range []int{http.StatusForbidden, http.StatusBadRequest} {
		t.Run(fmt.Sprintf("HTTP %d", statusCode), func(t *testing.T) {
			original := networkRefreshTestNetworkJWT()
			renewed := networkRefreshTestNetworkJWT()
			fake := newFakeNetworkAuthServer(t, renewed)
			fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(statusCode)
				_, _ = w.Write([]byte(`{"error":{"message":"refused"}}`))
			}

			got, err := refreshJWT(context.Background(), fake.srv.URL, original)
			if err != nil {
				t.Fatalf("refreshJWT: %v", err)
			}
			if got != renewed {
				t.Fatalf("refreshed token = %q, want the legacy code-login token", got)
			}
			if got := fake.log.count("/auth/code-create"); got != 1 {
				t.Fatalf("code-create requests = %d, want 1 after a refused network-refresh", got)
			}
		})
	}
}

func TestRefreshJWTFallsBackWhenNetworkRefreshReturnsAnError(t *testing.T) {
	original := networkRefreshTestNetworkJWT()
	renewed := networkRefreshTestNetworkJWT()
	fake := newFakeNetworkAuthServer(t, renewed)
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		writeNetworkRefreshJSON(t, w, map[string]any{
			"error": map[string]string{"message": "An API key does not expire and is not refreshed."},
		})
	}

	got, err := refreshJWT(context.Background(), fake.srv.URL, original)
	if err != nil {
		t.Fatalf("refreshJWT: %v", err)
	}
	if got != renewed {
		t.Fatalf("refreshed token = %q, want the legacy code-login token", got)
	}
	if got := fake.log.count("/auth/code-create"); got != 1 {
		t.Fatalf("code-create requests = %d, want 1 after a result error", got)
	}
}

func TestRefreshJWTSkipsNetworkRefreshForAClientTokenInTheFile(t *testing.T) {
	clientJWT := networkRefreshTestClientJWT()
	renewed := networkRefreshTestNetworkJWT()
	fake := newFakeNetworkAuthServer(t, renewed)

	got, err := refreshJWT(context.Background(), fake.srv.URL, clientJWT)
	if err != nil {
		t.Fatalf("refreshJWT: %v", err)
	}
	if got != renewed {
		t.Fatalf("refreshed token = %q, want the legacy code-login token", got)
	}
	if got := fake.log.count("/auth/network-refresh"); got != 0 {
		t.Fatalf("network-refresh requests = %d, want 0 for a client JWT", got)
	}
	if got := fake.log.count("/auth/code-create"); got != 1 {
		t.Fatalf("code-create requests = %d, want 1 (legacy behavior preserved)", got)
	}
	if got := fake.log.authFor("/auth/code-create"); got != clientJWT {
		t.Errorf("code-create carried %q, want the presented token (legacy behavior preserved)", got)
	}
}

func TestRefreshJWTDoesNotFallBackOnServerFailure(t *testing.T) {
	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("HTTP %d", statusCode), func(t *testing.T) {
			fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
			fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(statusCode)
			}

			got, err := refreshJWT(context.Background(), fake.srv.URL, networkRefreshTestNetworkJWT())
			if err == nil {
				t.Fatalf("refreshJWT succeeded on HTTP %d, want an error", statusCode)
			}
			if got != "" {
				t.Errorf("refreshed token = %q, want empty", got)
			}
			if got := fake.log.count("/auth/code-create"); got != 0 {
				t.Errorf("code-create requests = %d, want 0 after a server failure", got)
			}
		})
	}
}

func TestRefreshJWTDoesNotFallBackOnRateLimit(t *testing.T) {
	fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}

	_, err := refreshJWT(context.Background(), fake.srv.URL, networkRefreshTestNetworkJWT())
	if err == nil {
		t.Fatal("refreshJWT succeeded on HTTP 429, want an error")
	}
	if got := fake.log.count("/auth/code-create"); got != 0 {
		t.Errorf("code-create requests = %d, want 0 after a rate limit", got)
	}
}

func TestRefreshJWTDoesNotFallBackOnUnauthorized(t *testing.T) {
	fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}

	_, err := refreshJWT(context.Background(), fake.srv.URL, networkRefreshTestNetworkJWT())
	if err == nil {
		t.Fatal("refreshJWT succeeded on HTTP 401, want an error")
	}
	if got := fake.log.count("/auth/code-create"); got != 0 {
		t.Errorf("code-create requests = %d, want 0: the legacy flow needs the same dead token", got)
	}
}

func TestNetworkRefreshJWTRejectsAClientTokenResponse(t *testing.T) {
	fake := newFakeNetworkAuthServer(t, networkRefreshTestNetworkJWT())
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		writeNetworkRefreshJSON(t, w, map[string]any{"by_jwt": networkRefreshTestClientJWT()})
	}
	strategy := connect.NewClientStrategyWithDefaults(context.Background())

	_, fallback, err := networkRefreshJWT(context.Background(), strategy, fake.srv.URL, networkRefreshTestNetworkJWT())
	if err == nil {
		t.Fatal("networkRefreshJWT accepted a client JWT response")
	}
	if fallback {
		t.Errorf("fallback = true, want false: a client JWT response is a server regression, not a refusal")
	}
	if !strings.Contains(err.Error(), "regression guard") {
		t.Errorf("error = %v, want the regression guard message", err)
	}
}

func TestRefreshJWTRejectsARenewedTokenThatFailsVerification(t *testing.T) {
	original := networkRefreshTestNetworkJWT()
	renewed := networkRefreshTestNetworkJWT()
	fake := newFakeNetworkAuthServer(t, "legacy-token-should-not-be-used")
	fake.networkRefresh = func(w http.ResponseWriter, r *http.Request) {
		writeNetworkRefreshJSON(t, w, map[string]any{"by_jwt": renewed})
	}
	fake.transferStats = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}

	got, err := refreshJWT(context.Background(), fake.srv.URL, original)
	if err == nil {
		t.Fatal("refreshJWT accepted a token that failed verification")
	}
	if got != "" {
		t.Errorf("refreshed token = %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "fresh token rejected by server") {
		t.Errorf("error = %v, want the verification failure", err)
	}
	if got := fake.log.count("/auth/code-create"); got != 0 {
		t.Errorf("code-create requests = %d, want 0: a failed verification is not a fallback trigger", got)
	}
}
