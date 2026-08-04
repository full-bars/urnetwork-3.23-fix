package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docopt/docopt-go"
)

// unreachableApiUrl reliably fails to connect (port 0 cannot be dialed),
// simulating an http.Client.Do error such as a network failure.
const unreachableApiUrl = "http://127.0.0.1:0"

// runAndRecover executes f, catching any panic. It reports whether f
// panicked and, if so, the recovered value.
func runAndRecover(f func()) (panicked bool, panicVal interface{}) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			panicVal = r
		}
	}()
	f()
	return
}

// assertPanicsWithError fails the test unless f panicked with a value that
// implements the error interface. This mirrors the panic(err) calls in
// verifySend, verifyNetwork, loginNetwork, and clientId.
func assertPanicsWithError(t *testing.T, name string, f func()) {
	t.Helper()
	panicked, panicVal := runAndRecover(f)
	if !panicked {
		t.Fatalf("%s: expected panic, but none occurred", name)
	}
	if _, ok := panicVal.(error); !ok {
		t.Errorf("%s: panic value = %#v, want value implementing error", name, panicVal)
	}
}

func assertNoPanic(t *testing.T, name string, f func()) {
	t.Helper()
	panicked, panicVal := runAndRecover(f)
	if panicked {
		t.Fatalf("%s: unexpected panic: %v", name, panicVal)
	}
}

// truncatedBodyServer returns a server whose response declares a
// Content-Length larger than the bytes actually written, causing
// io.ReadAll(res.Body) to fail with an unexpected EOF.
func truncatedBodyServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("short"))
	}))
}

// invalidJSONServer returns a server that responds with a body that is not
// valid JSON, causing json.Unmarshal to fail.
func invalidJSONServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json"))
	}))
}

func TestVerifySend(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath, gotContentType string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"user_auth":"user@example.com"}`))
		}))
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
		}

		assertNoPanic(t, "verifySend", func() { verifySend(opts) })

		if gotMethod != http.MethodPost {
			t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotPath != "/auth/verify-send" {
			t.Errorf("request path = %q, want %q", gotPath, "/auth/verify-send")
		}
		if gotContentType != "application/json" {
			t.Errorf("request Content-Type = %q, want %q", gotContentType, "application/json")
		}
	})

	t.Run("invalid JSON response panics", func(t *testing.T) {
		srv := invalidJSONServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
		}

		assertPanicsWithError(t, "verifySend", func() { verifySend(opts) })
	})

	t.Run("truncated response body panics", func(t *testing.T) {
		srv := truncatedBodyServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
		}

		assertPanicsWithError(t, "verifySend", func() { verifySend(opts) })
	})

	t.Run("request failure panics", func(t *testing.T) {
		opts := docopt.Opts{
			"--api_url":   unreachableApiUrl,
			"--user_auth": "user@example.com",
		}

		assertPanicsWithError(t, "verifySend", func() { verifySend(opts) })
	})
}

func TestVerifyNetwork(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"network_id":"abc"}`))
		}))
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--code":      "123456",
		}

		assertNoPanic(t, "verifyNetwork", func() { verifyNetwork(opts) })

		if gotMethod != http.MethodPost {
			t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotPath != "/auth/verify" {
			t.Errorf("request path = %q, want %q", gotPath, "/auth/verify")
		}
	})

	t.Run("invalid JSON response panics", func(t *testing.T) {
		srv := invalidJSONServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--code":      "123456",
		}

		assertPanicsWithError(t, "verifyNetwork", func() { verifyNetwork(opts) })
	})

	t.Run("truncated response body panics", func(t *testing.T) {
		srv := truncatedBodyServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--code":      "123456",
		}

		assertPanicsWithError(t, "verifyNetwork", func() { verifyNetwork(opts) })
	})

	t.Run("request failure panics", func(t *testing.T) {
		opts := docopt.Opts{
			"--api_url":   unreachableApiUrl,
			"--user_auth": "user@example.com",
			"--code":      "123456",
		}

		assertPanicsWithError(t, "verifyNetwork", func() { verifyNetwork(opts) })
	})
}

func TestLoginNetwork(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"by_jwt":""}`))
		}))
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--password":  "s3cret",
		}

		assertNoPanic(t, "loginNetwork", func() { loginNetwork(opts) })

		if gotMethod != http.MethodPost {
			t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotPath != "/auth/login-with-password" {
			t.Errorf("request path = %q, want %q", gotPath, "/auth/login-with-password")
		}
	})

	t.Run("invalid JSON response panics", func(t *testing.T) {
		srv := invalidJSONServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--password":  "s3cret",
		}

		assertPanicsWithError(t, "loginNetwork", func() { loginNetwork(opts) })
	})

	t.Run("truncated response body panics", func(t *testing.T) {
		srv := truncatedBodyServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url":   srv.URL,
			"--user_auth": "user@example.com",
			"--password":  "s3cret",
		}

		assertPanicsWithError(t, "loginNetwork", func() { loginNetwork(opts) })
	})

	t.Run("request failure panics", func(t *testing.T) {
		opts := docopt.Opts{
			"--api_url":   unreachableApiUrl,
			"--user_auth": "user@example.com",
			"--password":  "s3cret",
		}

		assertPanicsWithError(t, "loginNetwork", func() { loginNetwork(opts) })
	})
}

func TestClientId(t *testing.T) {
	const testJwt = "test.jwt.token"

	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"by_client_jwt":""}`))
		}))
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url": srv.URL,
			"--jwt":     testJwt,
		}

		assertNoPanic(t, "clientId", func() { clientId(opts) })

		if gotMethod != http.MethodPost {
			t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
		}
		if gotPath != "/network/auth-client" {
			t.Errorf("request path = %q, want %q", gotPath, "/network/auth-client")
		}
		wantAuth := "Bearer " + testJwt
		if gotAuth != wantAuth {
			t.Errorf("request Authorization header = %q, want %q", gotAuth, wantAuth)
		}
	})

	t.Run("invalid JSON response panics", func(t *testing.T) {
		srv := invalidJSONServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url": srv.URL,
			"--jwt":     testJwt,
		}

		assertPanicsWithError(t, "clientId", func() { clientId(opts) })
	})

	t.Run("truncated response body panics", func(t *testing.T) {
		srv := truncatedBodyServer()
		defer srv.Close()

		opts := docopt.Opts{
			"--api_url": srv.URL,
			"--jwt":     testJwt,
		}

		assertPanicsWithError(t, "clientId", func() { clientId(opts) })
	})

	t.Run("request failure panics", func(t *testing.T) {
		opts := docopt.Opts{
			"--api_url": unreachableApiUrl,
			"--jwt":     testJwt,
		}

		assertPanicsWithError(t, "clientId", func() { clientId(opts) })
	})
}
