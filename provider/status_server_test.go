package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// TestStatusServerConfig_HasSlowlorisTimeouts guards the exact change made
// to provide()'s statusServer construction: ReadHeaderTimeout and
// IdleTimeout were added (matching hub/main.go) to defend against
// Slowloris-style connection exhaustion. The statusServer is built inline
// inside provide(), a function with many side effects (it starts real
// listeners, blocks on a WaitGroup, and eventually calls os.Exit), so it
// can't be invoked directly in a unit test. Instead this inspects the
// source to make sure the hardening fields stay in place.
func TestStatusServerConfig_HasSlowlorisTimeouts(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	mainPath := filepath.Join(filepath.Dir(thisFile), "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}

	block := regexp.MustCompile(`(?s)statusServer\s*:=\s*&http\.Server\{(.*?)\n\s*go func`).FindStringSubmatch(string(src))
	if len(block) < 2 {
		t.Fatal("could not locate the statusServer construction in provider/main.go")
	}

	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`Handler:\s*&Status\{\}`),
		regexp.MustCompile(`ReadHeaderTimeout:\s*10\s*\*\s*time\.Second`),
		regexp.MustCompile(`IdleTimeout:\s*120\s*\*\s*time\.Second`),
	} {
		if !re.MatchString(block[1]) {
			t.Errorf("statusServer construction no longer matches %s\nblock:\n%s", re.String(), block[1])
		}
	}
}

// TestStatusHandler_ServesOKBehindHardenedServer is a regression check that
// adding ReadHeaderTimeout/IdleTimeout to the server wrapping Status does
// not interfere with ordinary request handling.
func TestStatusHandler_ServesOKBehindHardenedServer(t *testing.T) {
	ts := httptest.NewUnstartedServer(&Status{})
	ts.Config.ReadHeaderTimeout = 10 * time.Second
	ts.Config.IdleTimeout = 120 * time.Second
	ts.Start()
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var result struct {
		Status string `json:"status"`
		Host   string `json:"host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("status field = %q, want %q", result.Status, "ok")
	}
}

// TestStatusServer_ReadHeaderTimeoutDropsSlowHeaderClients demonstrates the
// mechanism the PR relies on: a client that sends a request line and then
// withholds the rest of the headers (a Slowloris-style dribble) gets its
// connection closed once ReadHeaderTimeout elapses, instead of tying up a
// server goroutine indefinitely. A short timeout is used here purely to
// keep the test fast; the production value is 10s.
func TestStatusServer_ReadHeaderTimeoutDropsSlowHeaderClients(t *testing.T) {
	ts := httptest.NewUnstartedServer(&Status{})
	ts.Config.ReadHeaderTimeout = 100 * time.Millisecond
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Request line plus one header, then no terminating blank line —
	// withholding it for longer than ReadHeaderTimeout.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n")); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	time.Sleep(400 * time.Millisecond)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)

	// Either outcome demonstrates the connection was not left open
	// indefinitely: the server may respond "408 Request Timeout" before
	// closing, or it may close the connection outright (surfacing as EOF
	// or a reset on the client side).
	switch {
	case n > 0:
		if !bytes.Contains(buf[:n], []byte("408")) {
			t.Errorf("expected a 408 response after ReadHeaderTimeout, got: %q", buf[:n])
		}
	case err == nil:
		t.Error("expected an error or a 408 response once ReadHeaderTimeout elapsed, got neither")
	}
}